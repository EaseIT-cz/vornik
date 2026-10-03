package agentadmin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
)

// tree is an in-memory configs directory plus the approval tables: it
// applies a change's ops the way the apply engine would, and rebuilds State
// through the registry's own parsers, the way the live daemon does.
type tree struct {
	t         *testing.T
	ns        string
	r         *Renderer
	files     map[string]string
	approvals map[string]map[string]IntegrationApproval
	reach     map[string]string
	ceiling   float64
	homes     map[string]bool
	advert    map[string][]string
	writesOff bool
	// pending are filed, undecided changes: their locks hold and their
	// AddsUSD feed the conditional ceiling figure, as lockPending does.
	pending []Change
	// models is the classified catalogue and destinations the namespace's
	// approved model destinations (design §18.6 item 2).
	models       map[string]CatalogueModel
	destinations map[string]bool
}

func newTree(t *testing.T, ns string) *tree {
	t.Helper()
	r, err := NewRenderer(os.DirFS("../../configs/agent-templates"))
	if err != nil {
		t.Fatal(err)
	}
	return &tree{t: t, ns: ns, r: r, files: map[string]string{},
		approvals: map[string]map[string]IntegrationApproval{}, reach: map[string]string{},
		ceiling: 10, homes: map[string]bool{}, advert: map[string][]string{}}
}

func (tr *tree) state() *State {
	tr.t.Helper()
	st := &State{
		Namespace: tr.ns, DefaultBudgetUSD: 2, CeilingUSD: tr.ceiling, AgentImage: "ghcr.io/easeit-cz/vornik-agent:latest",
		Projects: map[string]*ProjectState{}, Workflows: map[string]*WorkflowState{},
		FileHashes: map[string]string{}, Locked: map[string]bool{}, Approvals: tr.approvals, Advertised: tr.advert,
		WritesOn: !tr.writesOff, models: tr.models, ApprovedDestinations: tr.destinations,
	}
	for _, c := range tr.pending {
		for _, l := range c.Locks {
			st.Locked[l] = true
		}
		if c.Grant.AddsUSD != nil && *c.Grant.AddsUSD > 0 {
			st.PendingAddsUSD += *c.Grant.AddsUSD
		}
	}
	swarms := map[string]*registry.Swarm{}
	for path, content := range tr.files {
		st.FileHashes[path] = HashContent([]byte(content))
		if strings.HasPrefix(path, "swarms/") {
			sw, err := registry.ParseSwarmMarkdown([]byte(content), path)
			if err != nil {
				tr.t.Fatalf("%s does not parse: %v\n%s", path, err, content)
			}
			swarms[sw.ID] = sw
		}
	}
	for path, content := range tr.files {
		switch {
		case strings.HasPrefix(path, "projects/"):
			var p registry.Project
			if err := yaml.Unmarshal([]byte(content), &p); err != nil {
				tr.t.Fatalf("%s does not parse: %v\n%s", path, err, content)
			}
			if err := p.Validate(path); err != nil {
				tr.t.Fatalf("%s does not validate: %v\n%s", path, err, content)
			}
			ps := ProjectStateFrom(&p, swarms[p.SwarmID])
			ps.Home = tr.homes[p.ID]
			st.Projects[p.ID] = ps
		case strings.HasPrefix(path, "workflows/"):
			wf, err := registry.ParseWorkflowMarkdown([]byte(content), path)
			if err != nil {
				tr.t.Fatalf("%s does not parse: %v\n%s", path, err, content)
			}
			st.Workflows[wf.ID] = &WorkflowState{ID: wf.ID, Loaded: wf, ApprovedReach: tr.reach[wf.ID]}
		}
	}
	for id, w := range st.Workflows {
		w.Project = ProjectOfWorkflow(id)
	}
	return st
}

func (tr *tree) render(verb string, in any) Change {
	tr.t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		tr.t.Fatal(err)
	}
	c, err := tr.r.Render(tr.state(), verb, raw)
	if err != nil {
		tr.t.Fatalf("%s: %v", verb, err)
	}
	return c
}

// apply writes a change's ops, checks its read set first, and records its
// grant, as approval plus apply would.
func (tr *tree) apply(c Change) {
	tr.t.Helper()
	if c.Class == Refused {
		tr.t.Fatalf("applying a refused change: %s", c.Reason)
	}
	for path, want := range c.ReadSet {
		cur, ok := tr.files[path]
		switch {
		case want == ReadSetAbsent && ok:
			tr.t.Fatalf("read set: %s exists", path)
		case want != ReadSetAbsent && (!ok || HashContent([]byte(cur)) != want):
			tr.t.Fatalf("read set: %s moved", path)
		}
	}
	// The ceiling as the widening_change effect decides it (§18.4), against
	// the tree before the ops.
	newCeiling := tr.ceiling
	if c.Class == Widening {
		st := tr.state()
		after, err := st.BudgetTotalAfter(c.Ops)
		if err != nil {
			tr.t.Fatal(err)
		}
		var ok bool
		if newCeiling, ok = CeilingAfter(c.Grant, st.CeilingUSD, st.BudgetTotal(), after); !ok {
			tr.t.Fatalf("applying %s: the sum $%v exceeds what its sentence stated", c.Verb, after)
		}
	}
	tr.ceiling = newCeiling
	for _, op := range c.Ops {
		if op.Op == OpDelete {
			delete(tr.files, op.Path)
			continue
		}
		tr.files[op.Path] = op.Content
	}
	for _, g := range c.Grant.Integrations {
		if tr.approvals[g.Project] == nil {
			tr.approvals[g.Project] = map[string]IntegrationApproval{}
		}
		tr.approvals[g.Project][g.Name] = IntegrationApproval{Kind: g.Kind, URL: g.URL, Read: g.Read, Write: g.Write, ReadPending: g.ReadPending}
	}
	for id, h := range c.Grant.Workflows {
		tr.reach[id] = h
	}
	for _, m := range c.Grant.Models {
		if tr.destinations == nil {
			tr.destinations = map[string]bool{}
		}
		tr.destinations[m.Destination] = true
	}
	for _, ref := range c.Narrow.RemovedIntegrations {
		a := tr.approvals[ref.Project][ref.Name]
		a.Removed = true
		tr.approvals[ref.Project][ref.Name] = a
	}
	for _, rm := range c.Narrow.RemovedTools {
		a := tr.approvals[rm.Project][rm.Name]
		a.Read = minus(a.Read, rm.Tools)
		tr.approvals[rm.Project][rm.Name] = a
	}
}

func (tr *tree) mustClass(c Change, want Class) {
	tr.t.Helper()
	if c.Class != want {
		tr.t.Fatalf("%s: class %s, want %s (reason %q, sentence %q)", c.Verb, c.Class, want, c.Reason, c.Sentence)
	}
}
