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

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

type operatorCredentials struct {
	c *Container
	// audit is the admin_audit repository; nil skips the row, as the page did.
	audit persistence.AdminAuditRepository
	// refreshDaemon and refreshProjects are the two re-registrations a config
	// reload runs; fields so a test can observe each.
	refreshDaemon   func()
	refreshProjects func(context.Context) error
}

func (c *Container) newOperatorCredentials() ui.OperatorCredentials {
	oc := &operatorCredentials{c: c, refreshDaemon: c.refreshMCPRegistry}
	if c.repos != nil {
		oc.audit = c.repos.AdminAudit
	}
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

func (o *operatorCredentials) Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	st, err := o.c.secretStoreForWrite()
	if err != nil {
		return fmt.Errorf("operator credentials: store unavailable: %w", err)
	}
	if err := st.PutOperatorCredential(ctx, name, value, who.Principal); err != nil {
		return err
	}
	o.auditWrite(ctx, "credential.set", name, who)
	return nil
}

func (o *operatorCredentials) Delete(ctx context.Context, name string, who persistence.AdminAuditEntry) error {
	src := o.c.currentSecretSource()
	if src.KeyErr != nil {
		return src.KeyErr
	}
	if src.Store == nil {
		// No store yet means nothing was ever stored: nothing to remove.
		return nil
	}
	if err := src.Store.DeleteOperatorCredential(ctx, name); err != nil {
		return err
	}
	o.auditWrite(ctx, "credential.remove", name, who)
	return nil
}

// auditWrite records one credential write after it succeeded: the action, the
// name and the caller, never the value. A failed audit write is logged and
// does not undo the store write (GitHub #625; the page's behaviour).
func (o *operatorCredentials) auditWrite(ctx context.Context, action, name string, who persistence.AdminAuditEntry) {
	if o.audit == nil {
		return
	}
	who.Action, who.Target = action, name
	if err := o.audit.Insert(ctx, &who); err != nil {
		o.c.Logger.Error().Err(err).Str("credential", name).Str("action", action).Msg("operator credentials: audit write failed")
	}
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

// operatorCredentialsAPI adapts the page's service to the REST verb's shapes
// (GitHub #625 item 2): one store, one audit, one refresh.
type operatorCredentialsAPI struct{ oc *operatorCredentials }

func (c *Container) newOperatorCredentialsAPI() api.OperatorCredentialsService {
	return operatorCredentialsAPI{oc: c.newOperatorCredentials().(*operatorCredentials)}
}

func (a operatorCredentialsAPI) List(ctx context.Context) (api.OperatorCredentialList, error) {
	view, err := a.oc.Status(ctx)
	if err != nil {
		return api.OperatorCredentialList{}, err
	}
	out := api.OperatorCredentialList{KeyUnusable: view.KeyUnusable, Credentials: []api.OperatorCredentialInfo{}}
	for _, r := range view.Rows {
		info := api.OperatorCredentialInfo{Name: r.Name, State: apiCredentialState(r), SetBy: r.StoreSetBy, NeededBy: r.NeededBy}
		if r.InStore && !r.StoreSetAt.IsZero() {
			at := r.StoreSetAt
			info.SetAt = &at
		}
		out.Credentials = append(out.Credentials, info)
	}
	return out, nil
}

func apiCredentialState(r ui.CredentialRow) string {
	switch {
	case r.InStore && r.InEnv:
		return "both"
	case r.InStore:
		return "stored"
	case r.InEnv:
		return "environment"
	}
	return "missing"
}

func (a operatorCredentialsAPI) Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	return a.oc.Put(ctx, name, value, who)
}

// Delete reports removed from the store's own list before the removal: a name
// held only in the environment is not removed (design round 2, review a155 F4).
func (a operatorCredentialsAPI) Delete(ctx context.Context, name string, who persistence.AdminAuditEntry) (bool, error) {
	view, err := a.oc.Status(ctx)
	if err != nil {
		return false, err
	}
	stored := false
	for _, r := range view.Rows {
		if r.Name == name && r.InStore {
			stored = true
		}
	}
	if !stored {
		return false, nil
	}
	return true, a.oc.Delete(ctx, name, who)
}

func (a operatorCredentialsAPI) Refresh(ctx context.Context) (api.OperatorCredentialsRefresh, error) {
	rep, err := a.oc.Refresh(ctx)
	return api.OperatorCredentialsRefresh{DaemonPresent: rep.DaemonPresent, ProjectDials: rep.ProjectDials}, err
}
