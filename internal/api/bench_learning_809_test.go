package api

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): the bench daemon's three learning-study routes. Found at: n/a
// (feature). Test at: unit (the handlers against fakes; the harness drives
// them in the e2e bench).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/benchlearning"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

type fakeTriggers809 struct {
	persistence.WorkflowHealingTriggerRepository
	inserted []*persistence.HealingTrigger
}

func (f *fakeTriggers809) List(context.Context, persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
	return append([]*persistence.HealingTrigger{{ID: "other", MetricName: "failure_rate"}}, f.inserted...), nil
}

func (f *fakeTriggers809) Insert(_ context.Context, t *persistence.HealingTrigger) error {
	t.ID = "trg-1"
	f.inserted = append(f.inserted, t)
	return nil
}

type fakeInstincts809 struct {
	persistence.InstinctRepository
	rows []*persistence.Instinct
}

func (f *fakeInstincts809) List(context.Context, persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	return f.rows, nil
}

type fakeCandidates809 struct {
	persistence.WorkflowHealingCandidateRepository
	rows []*persistence.HealingCandidate
}

func (f *fakeCandidates809) List(_ context.Context, filter persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	var out []*persistence.HealingCandidate
	for _, c := range f.rows {
		if filter.Status == "" || c.Status == filter.Status {
			out = append(out, c)
		}
	}
	return out, nil
}

func benchServer809(t *testing.T, trigger, auto bool) (*Server, *fakeTriggers809) {
	t.Helper()
	g, _, err := benchlearning.Open(t.TempDir(), "database agentbench_test oid 1")
	if err != nil {
		t.Fatal(err)
	}
	tr := &fakeTriggers809{}
	s := &Server{
		adminSurfacePresent:  true,
		adminConfig:          config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}},
		benchConfig:          config.BenchConfig{Instance: true, Learning: config.BenchLearningConfig{Trigger: config.BenchLearningTrigger{Enabled: trigger}, Auto: auto}},
		benchGate:            g,
		healingTriggerRepo:   tr,
		instinctRepo:         &fakeInstincts809{rows: []*persistence.Instinct{{ID: "i-1", Status: "active", Confidence: 0.81}}},
		healingCandidateRepo: &fakeCandidates809{rows: []*persistence.HealingCandidate{{ID: "c-1", WorkflowID: "dev-pipeline", Status: persistence.HealingCandidatePromoted, CandidateGenomeHash: "g1"}, {ID: "c-2", Status: "trial_failed"}}},
	}
	return s, tr
}

func call809(h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", bytes.NewReader([]byte(body)))
	r = r.WithContext(context.WithValue(r.Context(), apiKeyKey, "sk-admin"))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestBench809_NotABenchAnswersNotFound(t *testing.T) {
	s := &Server{}
	for _, h := range []http.HandlerFunc{s.AdminBenchFreeze, s.AdminBenchLearnedState, s.AdminBenchCreateTrigger} {
		if w := call809(h, http.MethodPost, `{}`); w.Code != http.StatusNotFound {
			t.Fatalf("a daemon that is not a bench answered %d %s", w.Code, w.Body.String())
		}
	}
}

func TestBench809_CreateTriggerThenFreezeRefusesIt(t *testing.T) {
	s, tr := benchServer809(t, true, true)
	body := `{"project_id":"agentbench","workflow_id":"dev-pipeline","failure_rate":0.5,"threshold":0.3,"runs":6,"evidence_execution_ids":["e1","e2"]}`
	w := call809(s.AdminBenchCreateTrigger, http.MethodPost, body)
	if w.Code != http.StatusCreated || len(tr.inserted) != 1 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	got := tr.inserted[0]
	if got.MetricName != BenchTriggerMetric || got.TriggerClass != persistence.HealingTriggerFailureRateSpike ||
		got.Status != persistence.HealingTriggerStatusOpen || len(got.EvidenceExecutionIDs) != 2 || got.ComparisonValue != 0.5 {
		t.Fatalf("trigger row = %+v", got)
	}
	if w := call809(s.AdminBenchFreeze, http.MethodPost, ``); w.Code != http.StatusOK {
		t.Fatalf("freeze: %d %s", w.Code, w.Body.String())
	}
	if w := call809(s.AdminBenchCreateTrigger, http.MethodPost, body); w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("LEARNING_FROZEN")) {
		t.Fatalf("a trigger after the freeze: %d %s", w.Code, w.Body.String())
	}
}

func TestBench809_CreateTriggerNeedsTheTriggerArm(t *testing.T) {
	s, _ := benchServer809(t, false, false)
	if w := call809(s.AdminBenchCreateTrigger, http.MethodPost, `{"project_id":"p","workflow_id":"w","evidence_execution_ids":["e"]}`); w.Code != http.StatusConflict {
		t.Fatalf("an arm without the bench trigger accepted one: %d %s", w.Code, w.Body.String())
	}
}

func TestBench809_LearnedStateHashesInstinctsAndPromotions(t *testing.T) {
	s, _ := benchServer809(t, true, true)
	var st benchLearnedState
	w := call809(s.AdminBenchLearnedState, http.MethodGet, ``)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil {
		t.Fatalf("learned state: %d %s", w.Code, w.Body.String())
	}
	if !st.TriggerEnabled || !st.Auto || st.Frozen || len(st.Instincts) != 1 || len(st.Promoted) != 1 || st.Promoted[0].CandidateID != "c-1" || st.SHA256 == "" {
		t.Fatalf("learned state = %+v", st)
	}
	if len(st.Triggers) != 0 {
		t.Fatalf("triggers = %+v, want only bench triggers (none yet)", st.Triggers)
	}
	if len(st.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want both, with their status", st.Candidates)
	}
	first := st.SHA256
	s.instinctRepo.(*fakeInstincts809).rows[0].Confidence = 0.9
	w = call809(s.AdminBenchLearnedState, http.MethodGet, ``)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.SHA256 == first {
		t.Fatal("a changed confidence did not change the learned-state hash")
	}
}

// GitHub #890 (design §12.37, part 3): each promotion reports the SHA-256 of
// the file it applied, so the measured pass's check 3c can accept exactly
// that file and refuse a hand edit or a leftover promotion.
func TestBench890_LearnedStateReportsThePromotedFileHash(t *testing.T) {
	s, _ := benchServer809(t, true, true)
	at := time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC)
	s.healingCandidateRepo.(*fakeCandidates809).rows[0].ProposalID = "wp-1"
	s.healingCandidateRepo.(*fakeCandidates809).rows[0].PromotedAt = &at
	body := "---\nworkflowId: dev-pipeline\n---\nrepaired\n"
	s.workflowProposals = &stubProposalRepo{getResult: &persistence.WorkflowProposal{ID: "wp-1", WorkflowID: "dev-pipeline", ProposalYAML: body}}
	var st benchLearnedState
	w := call809(s.AdminBenchLearnedState, http.MethodGet, ``)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || len(st.Promoted) != 1 {
		t.Fatalf("learned state: %d %s", w.Code, w.Body.String())
	}
	sum := sha256.Sum256([]byte(body))
	if got, want := st.Promoted[0].FileSHA256, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("file_sha256 = %q, want the SHA-256 of the proposal's YAML %q", got, want)
	}
	if st.Promoted[0].PromotedAt == nil || !st.Promoted[0].PromotedAt.Equal(at) {
		t.Fatalf("promoted_at = %v, want %v", st.Promoted[0].PromotedAt, at)
	}

	// No proposal to read: no hash, so the guard refuses rather than accepts.
	s.workflowProposals = &stubProposalRepo{getErr: persistence.ErrNotFound}
	w = call809(s.AdminBenchLearnedState, http.MethodGet, ``)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Promoted[0].FileSHA256 != "" {
		t.Fatalf("an unreadable proposal must leave file_sha256 empty, got %q", st.Promoted[0].FileSHA256)
	}
}
