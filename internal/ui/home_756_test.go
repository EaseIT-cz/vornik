package ui

// Home and the Diagnostics overview (ui-design-refresh-design.md §14, #756).
// Before #756 the home page carried eleven blocks, six of them engineering
// views (response cache, autonomy ETAs, memory pipeline, learning, judge,
// model leaderboard), and no Ask box, no "done today", no recent results.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

var engineeringTiles = []string{"Memory pipeline", "Judge · last 24h", "Autonomy · next eval", "Model Leaderboard", `data-tile="learning"`, `id="response-cache-card"`}

func homeExecRepo(done int64, recent []*persistence.Execution, seen *persistence.ExecutionFilter) *mocks.MockExecutionRepository {
	return &mocks.MockExecutionRepository{
		CountFunc: func(_ context.Context, f persistence.ExecutionFilter) (int64, error) {
			if seen != nil {
				*seen = f
			}
			return done, nil
		},
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return recent, nil
		},
	}
}

func TestHome_756_IsTheDoorsNotTheTiles(t *testing.T) {
	recent := []*persistence.Execution{{ID: "e1", TaskID: "task-abc", ProjectID: "p1", Status: persistence.ExecutionStatusCompleted}}
	srv := overviewServer(overviewTasks(map[persistence.TaskStatus]int{persistence.TaskStatusAwaitingApproval: 1}),
		WithExecutionRepository(homeExecRepo(14, recent, nil)), WithActiveChatSource(&fakeActiveChatSource{count: 3}))
	body := renderOverview(t, srv)
	for _, want := range []string{`data-block="greeting"`, `action="/ui/ask"`, `name="q"`, `data-block="needs-you"`, `data-block="stats"`, `data-block="recent"`, "14 jobs", `href="/ui/tasks/task-abc"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %s", want)
		}
	}
	for _, tile := range engineeringTiles {
		if strings.Contains(body, tile) {
			t.Errorf("home still shows the engineering tile %s; it moved to /ui/diagnostics", tile)
		}
	}
	if !strings.Contains(body, "1 thing needs you. 14 jobs done today.") {
		t.Error("the greeting's status line should count what needs you")
	}
}

// Done today counts executions by completed_at since the daemon's local
// midnight (§14.2): the filter the home page hands the repository.
func TestHome_756_DoneTodayCountsSinceLocalMidnight(t *testing.T) {
	var seen persistence.ExecutionFilter
	srv := overviewServer(nil, WithExecutionRepository(homeExecRepo(3, nil, &seen)))
	// Take midnight before the request so a date rollover cannot flake it.
	before := time.Now()
	renderOverview(t, srv)
	if seen.CompletedSince == nil || seen.Status == nil || *seen.Status != persistence.ExecutionStatusCompleted {
		t.Fatalf("Done today must count COMPLETED executions with a CompletedSince bound; got %+v", seen)
	}
	midnight := time.Date(before.Year(), before.Month(), before.Day(), 0, 0, 0, 0, time.Local)
	if !seen.CompletedSince.Equal(midnight) {
		t.Errorf("CompletedSince = %v, want the daemon's local midnight %v", seen.CompletedSince, midnight)
	}
}

func TestDiagnosticsOverview_756_HasTheTiles(t *testing.T) {
	srv := overviewServer(overviewTasks(map[persistence.TaskStatus]int{persistence.TaskStatusRunning: 1}),
		WithActiveChatSource(&fakeActiveChatSource{count: 3}))
	rec := httptest.NewRecorder()
	srv.DiagnosticsOverview(rec, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Active Chats", "Autonomy · next eval", "Memory pipeline", "Judge · last 24h", `data-block="at-a-glance"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Diagnostics overview lacks %q", want)
		}
	}
	if !strings.Contains(body, `hx-get="/ui/diagnostics?`) {
		t.Error("the overview's live refresh should re-fetch itself")
	}
}

// The scoped-session case is TestDashboard_603_ScopedSessionSeesNoInstanceWideStats,
// retargeted at /ui/diagnostics in #756.

func TestNav_756_DiagnosticsLandsOnTheOverview(t *testing.T) {
	for _, a := range navModel() {
		if a.Key != "diagnostics" {
			continue
		}
		if a.Href != "/ui/diagnostics" || len(a.Dests) == 0 || a.Dests[0].Key != "diagnostics-overview" || a.Dests[0].Href != "/ui/diagnostics" {
			t.Errorf("Diagnostics should land on and list the overview first: %+v", a)
		}
	}
	if navAreaForPage("diagnostics-overview") != "diagnostics" {
		t.Error("pageToArea should map the overview to Diagnostics")
	}
}

func TestAsk_756_PassesTheQuestionOnEncoded(t *testing.T) {
	long := strings.Repeat("é", 2100)
	for _, tc := range []struct {
		q, want string
	}{
		{"hello", "/ui/projects/assistant/chat?q=hello"},
		{"a&b#c?d%e", "/ui/projects/assistant/chat?q=a%26b%23c%3Fd%25e"},
		{"see https://x.example/?a=1", "/ui/projects/assistant/chat?q=see+https%3A%2F%2Fx.example%2F%3Fa%3D1"},
		{"<script>alert(1)</script>", "/ui/projects/assistant/chat?q=%3Cscript%3Ealert%281%29%3C%2Fscript%3E"},
	} {
		h := askHandler(func() string { return "assistant" }, func(*http.Request, string) bool { return true })
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/ui/ask?q="+url.QueryEscape(tc.q), nil))
		if got := rec.Header().Get("Location"); got != tc.want {
			t.Errorf("q=%q: Location %q, want %q", tc.q, got, tc.want)
		}
	}
	// Cut to 2,000 runes, still valid UTF-8.
	h := askHandler(func() string { return "assistant" }, func(*http.Request, string) bool { return true })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/ui/ask?q="+url.QueryEscape(long), nil))
	q := locationQuery(t, rec.Header().Get("Location"))
	if utf8.RuneCountInString(q) != 2000 || !utf8.ValidString(q) {
		t.Errorf("an over-long q should be cut to 2,000 runes of valid UTF-8; got %d runes", utf8.RuneCountInString(q))
	}
	// No dispatcher: q is dropped with the redirect to the project list.
	h = askHandler(func() string { return "" }, func(*http.Request, string) bool { return true })
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/ui/ask?q=hello", nil))
	if got := rec.Header().Get("Location"); got != "/ui/projects" {
		t.Errorf("without a dispatcher: Location %q, want /ui/projects", got)
	}
}

// The chat page pre-fills its composer from q, escaped, and sends nothing.
func TestChat_756_PrefillsTheComposerFromQ(t *testing.T) {
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.ChatPage(rec, httptest.NewRequest(http.MethodGet, "/ui/projects/assistant/chat?q=%3Cscript%3Ex%3C%2Fscript%3E+hi", nil), "assistant")
	body := rec.Body.String()
	if !strings.Contains(body, "&lt;script&gt;x&lt;/script&gt; hi</textarea>") {
		t.Error("the composer should hold the escaped question")
	}
	if strings.Contains(body, "<script>x</script>") {
		t.Error("q must never render as markup")
	}
}

func locationQuery(t *testing.T, loc string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("q")
}

// The home page's blocks sit inside #dashboard-live, which re-fetches /ui/
// on fleet activity and every 30 s (design §14.2).
func TestHome_756_RefreshesItsOwnBlocks(t *testing.T) {
	body := renderOverview(t, overviewServer(nil))
	live := strings.Index(body, `id="dashboard-live"`)
	if live < 0 || !strings.Contains(body, `hx-get="/ui/?`) || !strings.Contains(body, `hx-trigger="sse:fleet-changed, every 30s"`) {
		t.Fatal("home lost its live refresh")
	}
	for _, block := range []string{`data-block="stats"`, `data-block="recent"`} {
		if i := strings.Index(body, block); i < live {
			t.Errorf("%s should sit inside #dashboard-live", block)
		}
	}
}
