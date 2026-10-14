package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #172 (2026-10-13): a refused agent-admin call returned its reason
// to the agent and left no row of its own, so an assistant probing its
// boundaries was invisible to the operator. Agent-administered design §23.2.

type memRefusals struct {
	mu   sync.Mutex
	rows []persistence.AgentAdminRefusal
	fail bool
}

func (m *memRefusals) Record(_ context.Context, r persistence.AgentAdminRefusal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("disk full")
	}
	m.rows = append(m.rows, r)
	return nil
}
func (m *memRefusals) ListRecent(context.Context, string, int) ([]persistence.AgentAdminRefusal, error) {
	return nil, nil
}
func (m *memRefusals) CountByRule(_ context.Context, ns string, since time.Time) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, r := range m.rows {
		if r.Namespace == ns && !r.CreatedAt.Before(since) {
			out[r.Rule]++
		}
	}
	return out, nil
}
func (m *memRefusals) CountByNamespace(context.Context, time.Time) (map[string]int, error) {
	return nil, nil
}

func (m *memRefusals) all() []persistence.AgentAdminRefusal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]persistence.AgentAdminRefusal(nil), m.rows...)
}

// refusingAgentAdmin answers every verb refused with a fixed rule.
type refusingAgentAdmin struct {
	fakeAgentAdmin
	rule string
}

func (f *refusingAgentAdmin) Do(_ context.Context, _ *persistence.APIKey, verb string, _ json.RawMessage) (agentadmin.Result, error) {
	f.calls = append(f.calls, verb)
	return agentadmin.Result{Effect: agentadmin.EffectRefused, Reason: verb + " refused: http://secret.example is not https", Rule: f.rule}, nil
}

func refusalServer(t *testing.T) (*Server, *memRefusals, string, string, *bool) {
	t.Helper()
	srv, keys, _ := newBrokerMCPServer(t)
	fake := &refusingAgentAdmin{rule: agentadmin.RuleURLNotHTTPS}
	enabled := true
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return enabled }
	repo := &memRefusals{}
	srv.agentRefusals = NewAgentRefusalLedger(repo, zerolog.Nop())
	plainRaw, _ := seedCompanionKey(t, keys, "memory-acme", nil)
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))
	return srv, repo, plainRaw, agentRaw, &enabled
}

func callToolRaw(t *testing.T, srv *Server, raw, tool string, args any) {
	t.Helper()
	b, _ := json.Marshal(args)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": tool, "arguments": m}), raw)
	srv.CompanionMCPHandler(httptest.NewRecorder(), req)
}

func TestAgentRefusals_EachPointWritesOneRowWithoutInput(t *testing.T) {
	srv, repo, plainRaw, agentRaw, _ := refusalServer(t)
	before := testutil.ToFloat64(agentRefusalsTotal.WithLabelValues(agentadmin.RuleToolNotOffered))

	callToolRaw(t, srv, agentRaw, agentadmin.VerbAddAPI, map[string]any{"base_url": "http://secret.example"})
	callToolRaw(t, srv, agentRaw, "recall", map[string]any{"query": "secret query"})
	callToolRaw(t, srv, agentRaw, "no_such_tool_secret", nil)
	callToolRaw(t, srv, agentRaw, "status", map[string]any{"task_id": "t1", "bogus_secret": 1})
	callToolRaw(t, srv, agentRaw, "delegate", map[string]any{"workflow": "mail-digest", "inputs": map[string]any{"since": "2026-09-28T00:00:00Z"}})
	// An ordinary companion key: nothing is recorded.
	callToolRaw(t, srv, plainRaw, "no_such_tool", nil)
	callToolRaw(t, srv, plainRaw, "status", map[string]any{"bogus": 1})

	rows := repo.all()
	want := []struct{ verb, rule string }{
		{agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS},
		{"recall", agentadmin.RuleToolNotOffered},
		{refusalUnknownTool, agentadmin.RuleToolNotOffered},
		{"status", agentadmin.RuleInvalidArguments},
		{"delegate", agentadmin.RuleDelegateNotDelegable},
	}
	require.Len(t, rows, len(want), "%+v", rows)
	for i, w := range want {
		r := rows[i]
		if r.Verb != w.verb || r.Rule != w.rule || r.Namespace != "hermes" || r.KeyID != "akey-agent" || r.ProjectID != "hermes--home" || r.ID == "" || r.CreatedAt.IsZero() {
			t.Errorf("row %d = %+v, want verb %s rule %s", i, r, w.verb, w.rule)
		}
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "secret") {
			t.Errorf("row %d carries input: %s", i, raw)
		}
	}
	if got := testutil.ToFloat64(agentRefusalsTotal.WithLabelValues(agentadmin.RuleToolNotOffered)) - before; got != 2 {
		t.Fatalf("tool_not_offered metric moved %v, want 2", got)
	}
}

// The guard is the key, not agent_admin.enabled: an admin verb is answered
// "unknown tool" while disabled and is not recorded, a gated memory tool is.
func TestAgentRefusals_GuardIsTheKeyNotTheSetting(t *testing.T) {
	srv, repo, _, agentRaw, enabled := refusalServer(t)
	*enabled = false
	callToolRaw(t, srv, agentRaw, agentadmin.VerbAddAPI, map[string]any{})
	callToolRaw(t, srv, agentRaw, "recall", map[string]any{"query": "x"})
	rows := repo.all()
	require.Len(t, rows, 1, "%+v", rows)
	require.Equal(t, "recall", rows[0].Verb)
}

// The scheduled firing calls brokerDelegable with a synthetic key; it is not
// a call by the agent and is neither recorded nor counted.
// The recorder is called only from the companion tools/call path and the
// companion delegate call site; brokerDelegable itself and
// FireScheduledBroker never reach it.
func TestAgentRefusals_ScheduledFiringIsNotRecorded(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	callers := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		{
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "recordAgentRefusal" {
						callers[fn.Name.Name] = true
					}
					return true
				})
			}
		}
	}
	require.Equal(t, map[string]bool{"handleCompanionToolCall": true, "companionAdminTool": true, "companionBrokerDelegate": true}, callers)
}

// The not-recorded side (design §23.2): a read verb's error, a delegate
// refused after brokerDelegable (inputs failing the schema), and an unknown
// bearer write no row.
func TestAgentRefusals_NotRecordedSide(t *testing.T) {
	srv, repo, _, agentRaw, _ := refusalServer(t)
	reg := srv.projectRegistry
	projects := map[string]*registry.Project{}
	for _, p := range reg.ListProjects() {
		projects[p.ID] = p
	}
	acme := *reg.GetProject("broker-acme")
	mail, home := acme, acme
	mail.ID, home.ID = "hermes--mail", "hermes--home"
	projects[mail.ID], projects[home.ID] = &mail, &home
	registry.SeedForTest(reg, projects)
	workflows := map[string]*registry.Workflow{}
	for _, w := range reg.ListWorkflows() {
		workflows[w.ID] = w
	}
	digest := *reg.GetWorkflow("mail-digest")
	digest.ID = "hermes--mail--digest"
	workflows[digest.ID] = &digest
	registry.SeedWorkflowsForTest(reg, workflows)

	// Control: the same workflow with valid inputs passes brokerDelegable.
	if _, _, err := srv.brokerDelegable(&persistence.APIKey{AgentAdmin: true, AgentNamespace: "hermes", ProjectID: "hermes--home"},
		delegateArgs{Workflow: digest.ID}); err != nil {
		t.Fatalf("fixture: the namespace workflow is not delegable: %v", err)
	}
	text, isErr := callTool(t, srv, agentRaw, "delegate", map[string]any{"workflow": digest.ID, "inputs": map[string]any{}})
	require.True(t, isErr, text)
	require.Contains(t, text, "fails required")
	callToolRaw(t, srv, agentRaw, agentadmin.VerbShowWorkflow, map[string]any{"project": "nope", "slug": "x"})
	callToolRaw(t, srv, "sk-vornik-not-a-key", "recall", map[string]any{"query": "x"})
	require.Empty(t, repo.all())
}

// The cap is counted in the table: a second ledger over the same rows (a
// restart, or another replica) still refuses the 61st write of the hour.
func TestAgentRefusals_CapSurvivesARestart(t *testing.T) {
	repo := &memRefusals{}
	now := time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC)
	key := &persistence.APIKey{ID: "k", ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	first := NewAgentRefusalLedger(repo, zerolog.Nop())
	first.now = func() time.Time { return now }
	for i := 0; i < agentRefusalRowsPerHour; i++ {
		first.Record(context.Background(), key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	}
	second := NewAgentRefusalLedger(repo, zerolog.Nop())
	second.now = func() time.Time { return now.Add(time.Minute) }
	second.Record(context.Background(), key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	require.Len(t, repo.all(), agentRefusalRowsPerHour)
	require.Equal(t, 1, second.Unwritten("hermes"))
}

func TestAgentRefusals_WriteFailureStillAnswersAndCounts(t *testing.T) {
	srv, repo, _, agentRaw, _ := refusalServer(t)
	repo.fail = true
	before := testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("write_failed"))
	text, isErr := callTool(t, srv, agentRaw, agentadmin.VerbAddAPI, map[string]any{})
	require.False(t, isErr)
	require.Contains(t, text, `"refused"`)
	require.Equal(t, before+1, testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("write_failed")))
	require.Equal(t, 1, srv.agentRefusals.Unwritten("hermes"))
}

func TestAgentRefusals_CapPerNamespacePerHour(t *testing.T) {
	repo := &memRefusals{}
	l := NewAgentRefusalLedger(repo, zerolog.Nop())
	now := time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	key := &persistence.APIKey{ID: "k", ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	other := &persistence.APIKey{ID: "k2", ProjectID: "codex--home", AgentAdmin: true, AgentNamespace: "codex"}
	before := testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("cap"))
	for i := 0; i < agentRefusalRowsPerHour+1; i++ {
		l.Record(context.Background(), key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	}
	l.Record(context.Background(), other, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	require.Len(t, repo.all(), agentRefusalRowsPerHour+1, "60 for hermes and 1 for codex")
	require.Equal(t, before+1, testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("cap")))
	require.Equal(t, 1, l.Unwritten("hermes"))
	require.Equal(t, 0, l.Unwritten("codex"))
	// The next clock hour opens a new window.
	now = now.Add(time.Hour)
	l.Record(context.Background(), key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	require.Len(t, repo.all(), agentRefusalRowsPerHour+2)
}

func TestAgentRefusals_DelegateRuleMapping(t *testing.T) {
	// By type, not text: a wrapped notDelegableError still maps, and an
	// error carrying the same words but another type does not.
	nd := error(notDelegableError{workflow: "x"})
	require.Equal(t, `BROKER_WORKFLOW: "x" is not delegable with this key`, nd.Error())
	require.Equal(t, agentadmin.RuleDelegateNotDelegable, delegateRefusalRule(fmt.Errorf("wrapped: %w", nd)))
	require.Equal(t, agentadmin.RuleDelegateRefused, delegateRefusalRule(errors.New(`BROKER_WORKFLOW: "x" is not delegable with this key`)))
	require.Equal(t, agentadmin.RuleDelegateRefused, delegateRefusalRule(errors.New(`workflow "x" not found`)))
}

// #334 audit 2026-10-14: a change whose own record failed after it applied
// (rollBackUnrecorded, design §23.4) is a daemon-side DB failure, not the
// agent's refusal. It was stamped apply_refused and so written to
// agent_admin_refusals and counted toward agent_refusal_burst. It now
// carries record_failed, which the ledger does not record: no row, no
// refusal metric. The agent still gets its truthful refused answer.
func TestAgentRefusals_RecordFailedIsNotTheAgentsRefusal(t *testing.T) {
	srv, repo, _, agentRaw, _ := refusalServer(t)
	srv.agentAdmin.(*refusingAgentAdmin).rule = agentadmin.RuleRecordFailed
	before := testutil.ToFloat64(agentRefusalsTotal.WithLabelValues(agentadmin.RuleRecordFailed))
	text, isErr := callTool(t, srv, agentRaw, agentadmin.VerbRemove, map[string]any{})
	require.False(t, isErr)
	require.Contains(t, text, `"refused"`, "the agent is still told the change was refused")
	require.Empty(t, repo.all(), "a daemon-side record failure must not land in the refusal ledger")
	require.Equal(t, before, testutil.ToFloat64(agentRefusalsTotal.WithLabelValues(agentadmin.RuleRecordFailed)))
	require.NotContains(t, agentadmin.RefusalRules, agentadmin.RuleRecordFailed, "record_failed is not a refusal rule the page lists")
}

// ctxRefusals honours its context like a real driver, and can hold one
// namespace's count open until released.
type ctxRefusals struct {
	memRefusals
	blockNS string
	entered chan struct{}
	release chan struct{}
}

func (c *ctxRefusals) CountByRule(ctx context.Context, ns string, since time.Time) (map[string]int, error) {
	if ns == c.blockNS {
		close(c.entered)
		<-c.release
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.memRefusals.CountByRule(ctx, ns, since)
}

func (c *ctxRefusals) Record(ctx context.Context, r persistence.AgentAdminRefusal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.memRefusals.Record(ctx, r)
}

// #334 audit 2026-10-14: Record ran the count and the write on the request
// context, so a client that disconnected after its refusal was answered
// lost the row. It now runs them on a detached context with a short bound.
func TestAgentRefusals_DisconnectedClientStillWritesTheRow(t *testing.T) {
	repo := &ctxRefusals{}
	l := NewAgentRefusalLedger(repo, zerolog.Nop())
	key := &persistence.APIKey{ID: "k", ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Record(ctx, key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	require.Len(t, repo.all(), 1, "a disconnected client must not lose the refusal row")
	require.Equal(t, 0, l.Unwritten("hermes"))
}

// #334 audit 2026-10-14: one process-wide mutex was held across the count
// and the write, so a slow namespace queued every other namespace behind
// it. The lock is now per namespace: codex records while hermes is held
// inside its count.
func TestAgentRefusals_NamespacesDoNotQueueBehindEachOther(t *testing.T) {
	repo := &ctxRefusals{blockNS: "hermes", entered: make(chan struct{}), release: make(chan struct{})}
	l := NewAgentRefusalLedger(repo, zerolog.Nop())
	hermes := &persistence.APIKey{ID: "k", ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	codex := &persistence.APIKey{ID: "k2", ProjectID: "codex--home", AgentAdmin: true, AgentNamespace: "codex"}
	held := make(chan struct{})
	go func() {
		defer close(held)
		l.Record(context.Background(), hermes, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	}()
	<-repo.entered // hermes is inside its count, holding its lock
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Record(context.Background(), codex, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(repo.release)
		<-held
		t.Fatal("codex's refusal waited behind hermes's lock")
	}
	close(repo.release)
	<-held
	require.Len(t, repo.all(), 2)
}

// Review 9726 finding 1 (#334 audit 2026-10-14): the page reads Unwritten
// while Record counts into it; both go through the ledger's mutex. Run
// under -race, this fails if either side stops taking it.
func TestAgentRefusals_UnwrittenReadRacesNoWrite(t *testing.T) {
	repo := &memRefusals{fail: true}
	l := NewAgentRefusalLedger(repo, zerolog.Nop())
	key := &persistence.APIKey{ID: "k", ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			l.Record(context.Background(), key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
		}()
		go func() { defer wg.Done(); _ = l.Unwritten("hermes") }()
	}
	wg.Wait()
	require.Equal(t, 4, l.Unwritten("hermes"))
}
