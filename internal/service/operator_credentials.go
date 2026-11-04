package service

// The service side of the console's credentials page (GitHub #623): lists
// operator credentials and the un-namespaced names the configuration
// references, writes and removes them in the encrypted store, and re-runs MCP
// registration so a server waiting for a credential comes up without a
// restart.
//
// see LLD § https://docs.vornik.io, Amendment 2026-11-02 (#623)

import (
	"context"
	"fmt"
	"sort"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/ui"
)

type operatorCredentials struct {
	c *Container
	// refreshDaemon and refreshProjects are the two re-registrations a config
	// reload runs; fields so a test can observe each.
	refreshDaemon   func()
	refreshProjects func(context.Context) error
}

func (c *Container) newOperatorCredentials() ui.OperatorCredentials {
	oc := &operatorCredentials{c: c, refreshDaemon: c.refreshMCPRegistry}
	if r := c.newAdminMCPRefresher(); r != nil {
		oc.refreshProjects = r.RefreshAll
	}
	return oc
}

func (o *operatorCredentials) Status(ctx context.Context) (ui.CredentialsView, error) {
	src := o.c.currentSecretSource()
	if src.KeyErr != nil {
		return ui.CredentialsView{KeyUnusable: true}, nil
	}
	rows := map[string]*ui.CredentialRow{}
	row := func(name string) *ui.CredentialRow {
		r := rows[name]
		if r == nil {
			r = &ui.CredentialRow{Name: name}
			rows[name] = r
		}
		return r
	}
	if src.Store != nil {
		metas, err := src.Store.ListOperatorCredentials(ctx)
		if err != nil {
			return ui.CredentialsView{}, err
		}
		for _, m := range metas {
			r := row(m.Name)
			r.InStore, r.StoreSetAt, r.StoreSetBy = true, m.UpdatedAt, m.CreatedByDevice
		}
	}
	for name, servers := range o.referencedNames() {
		row(name).NeededBy = servers
	}
	out := make([]ui.CredentialRow, 0, len(rows))
	for _, r := range rows {
		// The resolver's own environment source, so the page and the lookup
		// cannot disagree (code review F5).
		if src.Env != nil {
			_, r.InEnv = src.Env.Get(r.Name)
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ui.CredentialsView{Rows: out}, nil
}

// referencedNames maps every un-namespaced secret NAME the daemon-level and
// project MCP servers reference to the servers that need it. Namespaced names
// belong to agent namespaces and are entered on an approver device (§8.2).
func (o *operatorCredentials) referencedNames() map[string][]string {
	out := map[string][]string{}
	add := func(server string, a mcpauth.Auth) {
		for _, name := range a.SecretRefs() {
			if _, _, namespaced := mcpauth.SplitSecretName(name); namespaced {
				continue
			}
			out[name] = append(out[name], server)
		}
	}
	for _, s := range o.c.daemonMCPServers() {
		add(s.Name, s.Auth)
	}
	if o.c.Registry != nil {
		for _, p := range o.c.Registry.ListProjects() {
			for _, s := range p.MCP.Servers {
				add(p.ID+"/"+s.Name, s.Auth)
			}
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out
}

func (o *operatorCredentials) Put(ctx context.Context, name string, value []byte, actor string) error {
	st, err := o.c.secretStoreForWrite()
	if err != nil {
		return fmt.Errorf("operator credentials: store unavailable: %w", err)
	}
	return st.PutOperatorCredential(ctx, name, value, actor)
}

func (o *operatorCredentials) Delete(ctx context.Context, name string) error {
	src := o.c.currentSecretSource()
	if src.KeyErr != nil {
		return src.KeyErr
	}
	if src.Store == nil {
		// No store yet means nothing was ever stored: nothing to remove.
		return nil
	}
	return src.Store.DeleteOperatorCredential(ctx, name)
}

// Refresh runs the daemon-level registry rebuild (synchronous server set,
// background probe) and the projects' re-dial (synchronous), the two steps a
// config reload runs, and reports what is present.
func (o *operatorCredentials) Refresh(ctx context.Context) (ui.CredentialsRefresh, error) {
	var rep ui.CredentialsRefresh
	if o.refreshDaemon != nil {
		o.refreshDaemon()
	}
	for _, s := range o.c.daemonMCPServerConfigs() {
		rep.DaemonPresent = append(rep.DaemonPresent, s.Name)
	}
	if o.refreshProjects == nil {
		return rep, nil
	}
	if err := o.refreshProjects(ctx); err != nil {
		return rep, err
	}
	if o.c.Registry == nil {
		return rep, nil
	}
	for id, servers := range o.c.mcpDesiredServers() {
		for _, s := range servers {
			rep.ProjectDials = append(rep.ProjectDials, id+"/"+s.Name+": re-dialled")
		}
	}
	sort.Strings(rep.ProjectDials)
	return rep, nil
}
