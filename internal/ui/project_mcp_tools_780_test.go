package ui

// GitHub #780 (control-plane hub design, amendment 2026-11-15): the managed
// instance's owner grants an MCP server's tools to their project's roles
// from /ui/projects/{id}/mcp-tools. The page files #627's proposal and, in
// the same request, approves it as the owner and applies it through the
// engine. Before, the owner could not reach the grant at all (the hub is
// admin-only). Found at: review (#627's design, "Not in this change"; an
// owner would see no way to use a server they added). Regression test at:
// integration (the console handler through the auth middleware, the real
// proposal store and apply engine over a temp config tree).

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
)

type toolsRig780 struct {
	useRig627
	keys  keysRig754
	audit *recordingAudit623
	busy  *bool
}

func newToolsRig780(t *testing.T, shared bool) toolsRig780 {
	t.Helper()
	r := newUseRig627(t, shared)
	busy := false
	// The engine as production wires it: proposals and the apply journal on
	// one database (container_controlplane.go), so the read set is checked.
	db := sqlitetest.Memory(t)
	r.repo = sqlite.NewProposalRepository(db.DB)
	WithProposalStore(r.repo)(r.srv)
	engine := &controlplane.ApplyEngine{Proposals: r.repo, ConfigDir: r.root, Reload: func() error { return nil }, Logger: zerolog.Nop(),
		HasActiveTasks: func(context.Context, string) (bool, error) { return busy, nil },
		Journal:        sqlite.NewApplyJournalRepository(db.DB)}
	audit := &recordingAudit623{}
	WithProposalApplier(engine)(r.srv)
	WithAdminAuditRepository(audit)(r.srv)
	return toolsRig780{useRig627: r, keys: keysRig754{srv: r.srv}, audit: audit, busy: &busy}
}

func (r toolsRig780) post(t *testing.T, who keysSessionBackend, project string, form url.Values) *http.Response {
	t.Helper()
	rec := r.keys.do(t, http.MethodPost, "/projects/"+project+"/mcp-tools", who, form)
	return rec.Result()
}

func (r toolsRig780) swarmAdmits(t *testing.T, role, tool string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, "configs", "swarms", "s1.md"))
	require.NoError(t, err)
	sw, err := registry.ParseSwarmMarkdown(b, "s1.md")
	require.NoError(t, err)
	for _, rr := range sw.Roles {
		if rr.Name == role {
			for _, e := range rr.Permissions.AllowedTools {
				if e == tool {
					return true
				}
			}
		}
	}
	return false
}

func grant780(ticks ...string) url.Values {
	f := url.Values{"server": {"scraper"}}
	for _, tk := range ticks {
		f.Add("grant", tk)
	}
	return f
}

func TestProjectMCPTools780_OwnerAppliesInOneStep(t *testing.T) {
	r := newToolsRig780(t, false)
	get := r.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil)
	require.Equal(t, http.StatusOK, get.Code)
	assert.Contains(t, get.Body.String(), `value="researcher|web_fetch"`)

	res := r.post(t, owner754, "alpha", grant780("lead|web_fetch", "researcher|web_fetch"))
	require.Equal(t, http.StatusSeeOther, res.StatusCode)
	assert.Contains(t, res.Header.Get("Location"), "done=applied")
	ps := r.proposals(t)
	require.Len(t, ps, 1)
	assert.Equal(t, persistence.ProposalStatusApplied, ps[0].Status)
	assert.Equal(t, "console:u-owner", ps[0].Approver)
	assert.Contains(t, ps[0].Evidence, `"source":"mcp-tools"`)
	assert.True(t, r.swarmAdmits(t, "researcher", "mcp__scraper__web_fetch"), "the applied grant reaches the swarm file")
	assert.True(t, r.swarmAdmits(t, "lead", "mcp__scraper__web_fetch"), "a kept grant stays")
	assert.False(t, r.swarmAdmits(t, "lead", "mcp__scraper__html_extract"), "an unticked tool is not granted")

	var rows []persistence.AdminAuditEntry
	for _, e := range r.audit.rows {
		if e.Action == "mcp.tools.grant" {
			rows = append(rows, e)
		}
	}
	require.Len(t, rows, 1)
	assert.Equal(t, "console:u-owner", rows[0].Principal)
	assert.Contains(t, rows[0].After, "applied")
}

func TestProjectMCPTools780_SharedSwarmNeedsTheAcknowledgement(t *testing.T) {
	r := newToolsRig780(t, true)
	res := r.post(t, owner754, "alpha", grant780("researcher|web_fetch"))
	assert.Contains(t, res.Header.Get("Location"), "done=ack-required")
	assert.False(t, r.swarmAdmits(t, "researcher", "mcp__scraper__web_fetch"), "nothing is written without the acknowledgement")
	assert.Empty(t, r.proposals(t), "nothing is filed without the acknowledgement, so Apply now can never skip it (review ba11 F1)")

	form := grant780("researcher|web_fetch")
	form.Set("ackShared", "on")
	res = r.post(t, owner754, "alpha", form)
	assert.Contains(t, res.Header.Get("Location"), "done=applied")
	assert.True(t, r.swarmAdmits(t, "researcher", "mcp__scraper__web_fetch"))
}

func TestProjectMCPTools780_BusyThenApplyNow(t *testing.T) {
	r := newToolsRig780(t, false)
	*r.busy = true
	res := r.post(t, owner754, "alpha", grant780("researcher|web_fetch"))
	assert.Contains(t, res.Header.Get("Location"), "done=busy")
	ps := r.proposals(t)
	require.Len(t, ps, 1)
	assert.Equal(t, persistence.ProposalStatusApproved, ps[0].Status)
	page := r.keys.do(t, http.MethodGet, "/projects/alpha/mcp-tools?server=scraper", owner754, nil).Body.String()
	assert.Contains(t, page, `name="id" value="`+ps[0].ID+`"`, "the pending proposal is listed with Apply now")

	*r.busy = false
	res = r.post(t, owner754, "alpha", url.Values{"action": {"apply"}, "id": {ps[0].ID}})
	assert.Contains(t, res.Header.Get("Location"), "done=applied")
	assert.True(t, r.swarmAdmits(t, "researcher", "mcp__scraper__web_fetch"))
}

func TestProjectMCPTools780_ApplyNowOnlyAppliesThisPagesProposals(t *testing.T) {
	r := newToolsRig780(t, true) // alpha and beta share s1
	other := &persistence.ControlPlaneProposal{ID: persistence.GenerateID("cpp"), Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, ProjectID: "alpha", Title: "hub edit", Status: persistence.ProposalStatusDraft,
		ProposedBy: "operator-ui", Evidence: `{"source":"hub"}`, ApplyTarget: "config.yaml", ApplyContent: "x: 1\n"}
	require.NoError(t, r.repo.Create(context.Background(), other))
	require.NoError(t, r.repo.SetStatus(context.Background(), other.ID, persistence.ProposalStatusApproved, "operator"))
	res := r.post(t, owner754, "alpha", url.Values{"action": {"apply"}, "id": {other.ID}})
	assert.Contains(t, res.Header.Get("Location"), "done=not-this-page")
	// A proposal with no source at all (a hub or older row) is not this page's.
	bare := &persistence.ControlPlaneProposal{ID: persistence.GenerateID("cpp"), Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, ProjectID: "alpha", Title: "bare", Status: persistence.ProposalStatusDraft,
		ProposedBy: "operator-ui", Evidence: `{"read_set":{}}`, ApplyTarget: "config.yaml", ApplyContent: "x: 1\n"}
	require.NoError(t, r.repo.Create(context.Background(), bare))
	require.NoError(t, r.repo.SetStatus(context.Background(), bare.ID, persistence.ProposalStatusApproved, "operator"))
	for _, action := range []string{"apply", "withdraw"} {
		res = r.post(t, owner754, "alpha", url.Values{"action": {action}, "id": {bare.ID}})
		assert.Contains(t, res.Header.Get("Location"), "done=not-this-page", action)
	}

	*r.busy = true
	form := grant780("researcher|web_fetch")
	form.Set("ackShared", "on")
	r.post(t, owner754, "beta", form)
	*r.busy = false
	var betaID string
	for _, p := range r.proposals(t) {
		if p.ProjectID == "beta" {
			betaID = p.ID
		}
	}
	require.NotEmpty(t, betaID)
	res = r.post(t, owner754, "alpha", url.Values{"action": {"apply"}, "id": {betaID}})
	assert.Contains(t, res.Header.Get("Location"), "done=not-this-page", "alpha's page applied beta's proposal")
	cfg, _ := os.ReadFile(filepath.Join(r.root, "config.yaml"))
	assert.False(t, strings.Contains(string(cfg), "x: 1"))
}

func TestProjectMCPTools780_WhoMayUseIt(t *testing.T) {
	r := newToolsRig780(t, false)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		var form url.Values
		if m == http.MethodPost {
			form = grant780("researcher|web_fetch")
		}
		rec := r.keys.do(t, m, "/projects/alpha/mcp-tools?server=scraper", member754, form)
		assert.Equal(t, http.StatusNotFound, rec.Code, "a project member: %s", m)
	}
	assert.Empty(t, r.proposals(t))
	// An admin (auth off: the trusted local operator) may use it too.
	res := r.post(t, keysSessionBackend{role: "admin", uid: "u-admin", projects: []string{"*"}}, "alpha", grant780("researcher|web_fetch"))
	assert.Contains(t, res.Header.Get("Location"), "done=applied")
}

func TestProjectMCPTools780_ForgedPostsAreRefused(t *testing.T) {
	r := newToolsRig780(t, false)
	for name, form := range map[string]url.Values{
		"a tool the registry does not list": grant780("lead|rm_rf"),
		"a role with no allowlist":          grant780("free|web_fetch"),
		"a pattern-covered role":            grant780("patterned|web_fetch"),
		"a bad server name":                 {"server": {"../x"}},
	} {
		res := r.post(t, owner754, "alpha", form)
		assert.Contains(t, res.Header.Get("Location"), "done=mcp-use-refused", name)
	}
	assert.Empty(t, r.proposals(t))
}

// Review ba11 F4, F5: a pending proposal can be withdrawn, and a file that
// changed while it waited makes its apply stale.
func TestProjectMCPTools780_WithdrawAndStale(t *testing.T) {
	r := newToolsRig780(t, false)
	*r.busy = true
	r.post(t, owner754, "alpha", grant780("researcher|web_fetch"))
	r.post(t, owner754, "alpha", grant780("researcher|html_extract"))
	ps := r.proposals(t)
	require.Len(t, ps, 2)
	*r.busy = false
	res := r.post(t, owner754, "alpha", url.Values{"action": {"withdraw"}, "id": {ps[0].ID}})
	assert.Contains(t, res.Header.Get("Location"), "done=withdrawn")
	got, err := r.repo.GetByID(context.Background(), ps[0].ID)
	require.NoError(t, err)
	assert.Equal(t, persistence.ProposalStatusRejected, got.Status)

	swarm := filepath.Join(r.root, "configs", "swarms", "s1.md")
	b, _ := os.ReadFile(swarm)
	require.NoError(t, os.WriteFile(swarm, append(b, []byte("\nEdited by hand.\n")...), 0o600))
	res = r.post(t, owner754, "alpha", url.Values{"action": {"apply"}, "id": {ps[1].ID}})
	assert.Contains(t, res.Header.Get("Location"), "done=stale-base")
}
