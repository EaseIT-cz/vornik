package api

// GitHub #908: every generate-candidate call in a bench blackbox arm failed.
// The config assistant writes a candidate in a multi-turn tool loop (one
// measured generation: six model calls in 60 s, a seventh cut off), and three
// deadlines below the loop's own 5m budget cut it: this handler's fixed 60 s,
// the daemon's http.Server WriteTimeout, and the autopilot's 30 s client.
//
// Found at: production (bench arm C exploratory run, 2026-11-24).
// Regression test at: integration (a real httptest server with a short
// WriteTimeout, through the access-log and metrics wrappers) and unit (the
// deadline the handler hands the producer).
// Lower because: a production reproduction needs a remote model and a
// minutes-long loop; the defect is which deadline each layer sets, which
// these tests read directly.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// deadlineHealingAssistant records the deadline the producer was given and
// can take a while, as the real tool loop does.
type deadlineHealingAssistant struct {
	mu       sync.Mutex
	proposal *persistence.ControlPlaneProposal
	delay    time.Duration
	deadline time.Time
	hasDL    bool
}

func (d *deadlineHealingAssistant) ProposeForHealing(ctx context.Context, _, _, _ string, _ []string) (*persistence.ControlPlaneProposal, error) {
	d.mu.Lock()
	d.deadline, d.hasDL = ctx.Deadline()
	d.mu.Unlock()
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return d.proposal, nil
}

func budget908Server(t *testing.T, requestTimeout string, a healingAssistant) *Server {
	t.Helper()
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	cfg := &config.Config{ConfigAssistant: config.AssistantConfig{RequestTimeout: requestTimeout}}
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo), WithConfig(cfg))
	s := NewServer(opts...)
	s.workflowProposals = &recordingWorkflowProposals{}
	s.SetHealingAssistant(a)
	return s
}

// The producer must be given the assistant's loop budget plus the margin, not
// the 60 s that cut the bench's generations.
func TestGenerateCandidate_ProducerGetsTheLoopBudgetNotSixtySeconds_908(t *testing.T) {
	a := &deadlineHealingAssistant{proposal: assistantHealingProposal(t, "wf-a", "---\nworkflowId: wf-a\n---\n# healed\n")}
	s := budget908Server(t, "5m", a)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil), "sk-admin")
	rec := httptest.NewRecorder()
	start := time.Now()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !a.hasDL {
		t.Fatal("the producer must run under a deadline")
	}
	left := a.deadline.Sub(start)
	if left <= 60*time.Second {
		t.Fatalf("the producer got %s; a tool loop needs more than the 60 s that cut 12 of 12 bench generations", left)
	}
	if want := 7 * time.Minute; left > want+time.Second || left < want-5*time.Second {
		t.Fatalf("the producer got %s, want the budget request_timeout + margin = %s", left, want)
	}
	if got := s.HealingGenerateBudget(); got != 7*time.Minute {
		t.Fatalf("HealingGenerateBudget = %s, want 7m", got)
	}
}

// A generation that outlasts the server's WriteTimeout must still deliver its
// answer: the handler lifts this one route's write deadline to the budget.
// Served through the access-log and metrics wrappers, because a write
// deadline reaches the connection only through writers that forward Unwrap.
func TestGenerateCandidate_OutlivesTheServerWriteTimeout_908(t *testing.T) {
	a := &deadlineHealingAssistant{
		proposal: assistantHealingProposal(t, "wf-a", "---\nworkflowId: wf-a\n---\n# healed\n"),
		delay:    400 * time.Millisecond,
	}
	s := budget908Server(t, "5s", a)
	s.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.AdminHealingTriggerGenerateCandidate(w, withAdminKeyContext(r, "sk-admin"), "t-1")
	})
	wrapped := AccessLogMiddleware(zerolog.Nop())(s.apiMetrics.Middleware(h))
	srv := httptest.NewUnstartedServer(wrapped)
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Post(srv.URL+"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", "application/json", nil)
	if err != nil {
		t.Fatalf("the server cut the response at its WriteTimeout: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The wrappers in the API chain forward Unwrap, so http.ResponseController
// reaches the connection through them (this also makes the owner export's
// write deadline take effect).
func TestResponseWrappersForwardWriteDeadline_908(t *testing.T) {
	m := NewAPIMetrics(prometheus.NewRegistry())
	var errs []error
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		errs = append(errs, http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)))
		w.WriteHeader(http.StatusNoContent)
	})
	for name, wrapped := range map[string]http.Handler{
		"access log": AccessLogMiddleware(zerolog.Nop())(h),
		"metrics":    m.Middleware(h),
	} {
		errs = nil
		srv := httptest.NewServer(wrapped)
		resp, err := srv.Client().Get(srv.URL + "/x")
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		srv.Close()
		if len(errs) != 1 || errs[0] != nil {
			t.Fatalf("%s wrapper: SetWriteDeadline = %v, want nil (forward Unwrap)", name, errs)
		}
	}
}
