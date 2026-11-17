package ui

// GitHub #627 (control-plane hub design, amendment 2026-11-15): "Use in a
// project" files one proposal that subscribes a project to an MCP server and
// lets chosen roles of its swarm call chosen tools, so a server added in the
// console can be put to use without a shell. Found at: production
// (operator, 2026-11-02: Context7 needed four hand edits). Regression test
// at: integration (the real handler, registry, YAML edits and apply engine
// over a temp config tree).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

const useSwarm627 = `---
swarmId: s1
roles:
  # The lead plans; it may fetch.
  - name: lead
    model: m
    systemPrompt: lead
    runtime:
      image: i
    permissions:
      allowedTools:
        - file_read
        - mcp__scraper__web_fetch
  - name: researcher
    model: m
    systemPrompt: research
    runtime:
      image: i
    permissions:
      allowedTools:
        - file_read
        - memory_search
  - name: free
    model: m
    systemPrompt: free
    runtime:
      image: i
  - name: patterned
    model: m
    systemPrompt: patterned
    runtime:
      image: i
    permissions:
      allowedTools:
        - mcp__scraper__*
---
`

const useWorkflow627 = `---
workflowId: w1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`

func useProject627(id, mcp string) string {
	return "# " + id + " banner\nprojectId: " + id + "\ndisplayName: " + id + "\nswarmId: s1\ndefaultWorkflowId: w1\nmaxConcurrentTasks: 1\n" + mcp
}

type useRig627 struct {
	srv  *Server
	repo persistence.ProposalRepository
	root string // holds config.yaml; the registry tree is root/configs
}

func newUseRig627(t *testing.T, sharedSwarm bool) useRig627 {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "configs")
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(cfg, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp:\n  servers:\n    - name: scraper\n      transport: sse\n      url: http://scraper.local/sse\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "swarms", "s1.md"), []byte(useSwarm627), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "workflows", "w1.md"), []byte(useWorkflow627), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "projects", "alpha.yaml"),
		[]byte(useProject627("alpha", "mcp:\n  servers:\n    - name: scraper\n      allowed_tools:\n        - web_fetch\n")), 0o600))
	if sharedSwarm {
		require.NoError(t, os.WriteFile(filepath.Join(cfg, "projects", "beta.yaml"), []byte(useProject627("beta", "")), 0o600))
	}
	reg := registry.New()
	require.NoError(t, reg.Load(cfg))
	repo := newProposalRepoUI(t)
	src := seededMCPSource() // scraper: web_fetch, html_extract
	srv := NewServer(WithProjectRegistry(reg), WithProposalStore(repo),
		WithControlPlaneConfigPath(filepath.Join(root, "config.yaml")), WithMCPFormRegistrySource(src))
	return useRig627{srv: srv, repo: repo, root: root}
}

func (r useRig627) get(t *testing.T, query string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.srv.AdminControlPlaneMCPUse(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane/mcp/use?"+query, nil))
	return rec.Code, rec.Body.String()
}

func (r useRig627) post(t *testing.T, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane/mcp/use", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.srv.AdminControlPlaneMCPUse(rec, req)
	return rec
}

func (r useRig627) proposals(t *testing.T) []*persistence.ControlPlaneProposal {
	t.Helper()
	ps, err := r.repo.List(context.Background(), persistence.ProposalListFilter{})
	require.NoError(t, err)
	return ps
}

func grant627(project string, ticks ...string) url.Values {
	f := url.Values{"server": {"scraper"}, "project": {project}}
	for _, tk := range ticks {
		f.Add("grant", tk) // "<role>|<tool>"
	}
	return f
}

func TestMCPUse627_PageShowsToolsGrantsAndTheSharedSwarm(t *testing.T) {
	r := newUseRig627(t, true)
	code, body := r.get(t, "server=scraper&project=alpha")
	require.Equal(t, http.StatusOK, code)
	for _, want := range []string{"web_fetch", "html_extract", "Fetch a URL",
		`value="lead|web_fetch" checked`, `value="researcher|html_extract"`,
		"this role declares no allowlist", "every tool, by pattern",
		"This swarm also serves beta"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, `value="free|`, "a role with no allowlist offers no boxes")
	assert.NotContains(t, body, `value="patterned|`, "a pattern-covered role offers no boxes")
}

func TestMCPUse627_UnreachableServerOffersNothing(t *testing.T) {
	r := newUseRig627(t, false)
	r.srv.mcpRegistrySource = &fakeMCPSource{servers: []MCPRegistryServer{{Name: "scraper", Reachable: false, Error: "down"}}}
	code, body := r.get(t, "server=scraper&project=alpha")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "has not listed its tools")
	assert.NotContains(t, body, `name="grant"`)
}

func TestMCPUse627_FilesOneTwoFileProposal(t *testing.T) {
	r := newUseRig627(t, false)
	rec := r.post(t, grant627("alpha", "lead|web_fetch", "researcher|web_fetch", "researcher|html_extract"))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	ps := r.proposals(t)
	require.Len(t, ps, 1)
	p := ps[0]
	assert.Equal(t, persistence.ProposalScopeProject, p.BlastRadius)
	assert.Equal(t, "operator-ui", p.ProposedBy)
	assert.False(t, p.LiveApply)
	var ops []struct{ Op, Path, Content string }
	require.NoError(t, json.Unmarshal([]byte(p.ApplyOps), &ops))
	require.Len(t, ops, 2)
	byPath := map[string]string{}
	for _, o := range ops {
		assert.Equal(t, "replace", o.Op)
		byPath[o.Path] = o.Content
	}
	project, swarm := byPath["configs/projects/alpha.yaml"], byPath["configs/swarms/s1.md"]
	require.NotEmpty(t, project, "ops: %v", byPath)
	require.NotEmpty(t, swarm)
	assert.Contains(t, project, "html_extract", "a ticked tool outside the narrowing joins allowed_tools")
	assert.Contains(t, project, "# alpha banner", "comments survive")
	assert.Contains(t, swarm, "# The lead plans; it may fetch.", "comments survive")
	assert.Contains(t, swarm, "- memory_search", "other entries stay")
	assert.Contains(t, swarm, "mcp__scraper__html_extract")
	var ev struct {
		ReadSet map[string]string `json:"read_set"`
		Actor   string            `json:"actor"`
	}
	require.NoError(t, json.Unmarshal([]byte(p.Evidence), &ev))
	assert.Len(t, ev.ReadSet, 2)
	assert.NotEmpty(t, ev.ReadSet["configs/swarms/s1.md"])
	assert.NotEmpty(t, ev.Actor)
	assert.Contains(t, p.Diff, "configs/swarms/s1.md")

	// The edited files load: the swarm parses and the role holds exactly
	// the server's ticked names, its other entries in place.
	sw, err := registry.ParseSwarmMarkdown([]byte(swarm), "s1.md")
	require.NoError(t, err)
	for _, role := range sw.Roles {
		switch role.Name {
		case "researcher":
			assert.Equal(t, []string{"file_read", "memory_search", "mcp__scraper__web_fetch", "mcp__scraper__html_extract"}, role.Permissions.AllowedTools)
		case "lead":
			assert.Equal(t, []string{"file_read", "mcp__scraper__web_fetch"}, role.Permissions.AllowedTools)
		case "free":
			assert.Empty(t, role.Permissions.AllowedTools)
		}
	}
}

func TestMCPUse627_SharedSwarmIsSwarmScope(t *testing.T) {
	r := newUseRig627(t, true)
	rec := r.post(t, grant627("beta", "lead|web_fetch"))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	p := r.proposals(t)[0]
	assert.Equal(t, persistence.ProposalScopeSwarm, p.BlastRadius)
	assert.Contains(t, p.Rationale, "alpha", "the rationale names the other project the swarm serves")
	var ops []struct{ Op, Path, Content string }
	require.NoError(t, json.Unmarshal([]byte(p.ApplyOps), &ops))
	for _, o := range ops {
		if o.Path == "configs/projects/beta.yaml" {
			assert.Contains(t, o.Content, "name: scraper", "an unsubscribed project is subscribed")
		}
	}
}

func TestMCPUse627_UntickRemovesOnlyThatServersNames(t *testing.T) {
	r := newUseRig627(t, false)
	rec := r.post(t, grant627("alpha")) // nothing ticked: lead loses web_fetch
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	var ops []struct{ Op, Path, Content string }
	require.NoError(t, json.Unmarshal([]byte(r.proposals(t)[0].ApplyOps), &ops))
	for _, o := range ops {
		if strings.HasSuffix(o.Path, "s1.md") {
			sw, err := registry.ParseSwarmMarkdown([]byte(o.Content), "s1.md")
			require.NoError(t, err)
			for _, role := range sw.Roles {
				if role.Name == "lead" {
					assert.Equal(t, []string{"file_read"}, role.Permissions.AllowedTools)
				}
				if role.Name == "patterned" {
					assert.Equal(t, []string{"mcp__scraper__*"}, role.Permissions.AllowedTools, "a pattern role is left as it is")
				}
			}
		}
	}
}

func TestMCPUse627_UnchangedFilesNothing(t *testing.T) {
	r := newUseRig627(t, false)
	rec := r.post(t, grant627("alpha", "lead|web_fetch"))
	assert.Contains(t, rec.Header().Get("Location"), "done=mcp-use-unchanged")
	assert.Empty(t, r.proposals(t))
}

func TestMCPUse627_RefusesForgedPosts(t *testing.T) {
	r := newUseRig627(t, false)
	for name, form := range map[string]url.Values{
		"a tool the registry does not list": grant627("alpha", "lead|rm_rf"),
		"a role with no allowlist":          grant627("alpha", "free|web_fetch"),
		"a pattern-covered role":            grant627("alpha", "patterned|web_fetch"),
		"an unknown role":                   grant627("alpha", "ghost|web_fetch"),
		"an unknown project":                grant627("nope", "lead|web_fetch"),
		"a bad server name":                 {"server": {"../x"}, "project": {"alpha"}},
	} {
		rec := r.post(t, form)
		assert.Contains(t, rec.Header().Get("Location"), "done=mcp-use-refused", name)
		assert.Empty(t, r.proposals(t), name)
	}
}

// The filed proposal, applied by the real engine over the temp tree, leaves
// a project and swarm the registry loads, and the granted role then admits
// the tool while no other role gains it.
func TestMCPUse627_AppliedProposalGrantsTheRole(t *testing.T) {
	r := newUseRig627(t, false)
	rec := r.post(t, grant627("alpha", "lead|web_fetch", "researcher|html_extract"))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	p := r.proposals(t)[0]
	require.NoError(t, r.repo.SetStatus(context.Background(), p.ID, persistence.ProposalStatusApproved, "operator"))
	engine := &controlplane.ApplyEngine{Proposals: r.repo, ConfigDir: r.root, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	require.NoError(t, engine.Apply(context.Background(), p.ID, "operator", false))

	reg := registry.New()
	require.NoError(t, reg.Load(filepath.Join(r.root, "configs")))
	proj := reg.GetProject("alpha")
	require.NotNil(t, proj)
	sw := reg.GetSwarm(proj.SwarmID)
	require.NotNil(t, sw)
	admits := map[string]bool{}
	for _, role := range sw.Roles {
		admits[role.Name] = len(role.Permissions.AllowedTools) > 0 && agenttools.AllowlistAdmits(role.Permissions.AllowedTools, "mcp__scraper__html_extract")
	}
	assert.True(t, admits["researcher"], "the granted role admits the tool")
	assert.False(t, admits["lead"], "a role that was not granted it does not")
	var subscribed bool
	for _, srv := range proj.MCP.Servers {
		if srv.Name == "scraper" {
			subscribed = true
			assert.Contains(t, srv.AllowedTools, "html_extract")
		}
	}
	assert.True(t, subscribed)
}

// The page is the admin router's: a non-admin session is refused before it
// is reached (the router's own admin re-check, admin.go).
func TestMCPUse627_NonAdminIsRefused(t *testing.T) {
	r := newUseRig627(t, false)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		req := scopedUIRequest(m, "/admin/control-plane/mcp/use?server=scraper&project=alpha", []string{"alpha"})
		rec := httptest.NewRecorder()
		r.srv.adminRouter(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, m)
	}
	assert.Empty(t, r.proposals(t))
}

// U3: the entry points link to the page.
func TestMCPUse627_EntryPoints(t *testing.T) {
	r := newUseRig627(t, false)
	rec := httptest.NewRecorder()
	r.srv.ProjectConfigFormEdit(rec, httptest.NewRequest(http.MethodGet, "/projects/alpha/config/form", nil), "alpha")
	// Since #780 the row links to the project page, which admins and the
	// owner both reach.
	assert.Contains(t, rec.Body.String(), "/ui/projects/alpha/mcp-tools?server=scraper")
}

// The managed instance's owner (tenant_admin) is refused too: the hub is not
// one of its surfaces (amendment 2026-11-15, "Not in this change"; code
// review 3032 F2). Through the real auth middleware and console handler.
func TestMCPUse627_TenantAdminIsRefused(t *testing.T) {
	r := newUseRig627(t, false)
	rig := keysRig754{srv: r.srv}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		var form url.Values
		if m == http.MethodPost {
			form = grant627("alpha", "researcher|web_fetch")
		}
		rec := rig.do(t, m, "/admin/control-plane/mcp/use?server=scraper&project=alpha", owner754, form)
		assert.Equal(t, http.StatusForbidden, rec.Code, m)
	}
	assert.Empty(t, r.proposals(t))
}
