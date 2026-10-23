package memory

// GitHub #482, design 22 "Amendment 2026-10-21 (#482)": an agent role in a
// companion-delegated task is never role-of-record. The ingest-level
// regression test is test/integration/provenance_ingest_482_test.go; these
// pin the rule and the pipeline's fail-closed branches.

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

func TestEffectiveClassPolicyFor_CompanionDelegated(t *testing.T) {
	for _, tc := range []struct {
		class     ContentClass
		role      string
		delegated bool
		rok       bool
	}{
		{ClassDecision, "reviewer", false, true},
		{ClassSpec, "rag-ingester", false, true},
		// Every role, not only the two the decision names.
		{ClassDecision, "reviewer", true, false},
		{ClassSpec, "rag-ingester", true, false},
		{ClassDecision, "architect", true, false},
		{ClassSpec, "analyst", true, false},
		{ClassSpec, "lead", true, false},
		{ClassDecision, "feasibility", true, false},
		{ClassDecision, "risk-officer", true, false},
		// A companion deposit stays capped either way.
		{ClassDecision, "companion:claude-code", false, false},
		{ClassDecision, "companion:claude-code", true, false},
	} {
		base := EffectiveClassPolicy(tc.class, tc.role)
		got := EffectiveClassPolicyFor(tc.class, tc.role, tc.delegated)
		if got.RoleOfRecordEligible != tc.rok {
			t.Errorf("%s/%s delegated=%v: role-of-record %v, want %v", tc.class, tc.role, tc.delegated, got.RoleOfRecordEligible, tc.rok)
		}
		// Delegation changes verification only, never confidence or expiry.
		if got.DefaultConfidence != base.DefaultConfidence || got.TTL != base.TTL {
			t.Errorf("%s/%s: delegation changed confidence or TTL: %+v vs %+v", tc.class, tc.role, got, base)
		}
	}
}

// Round-3 review A5 and B3. The early return before the origin lookup (a
// companion or chat producer) is exclusive with the gate: such a producer is
// never role-of-record for any class. And a companion-delegated agent chunk
// is never role-of-record for any class or role, so the only stamp it can
// carry is the provenance one.
func TestHumanDepositNeverEligible(t *testing.T) {
	classes := []ContentClass{ContentClass("incident"), ContentClass("")}
	for c := range DefaultClassPolicies {
		classes = append(classes, c)
	}
	roles := []string{"", "unknown-role"}
	for r := range roleClassMap {
		roles = append(roles, r)
	}
	for _, c := range classes {
		for _, human := range []string{"companion:claude-code", "companion:", "chat:telegram", "chat:"} {
			if EffectiveClassPolicy(c, human).RoleOfRecordEligible || EffectiveClassPolicyFor(c, human, false).RoleOfRecordEligible {
				t.Errorf("class %q, producer %q: role-of-record", c, human)
			}
		}
		for _, r := range roles {
			if EffectiveClassPolicyFor(c, r, true).RoleOfRecordEligible {
				t.Errorf("class %q, role %q, companion-delegated: role-of-record", c, r)
			}
		}
	}
}

type fakeProvenance struct {
	res   ProvenanceResult
	calls int
}

func (f *fakeProvenance) Verify(context.Context, ProvenanceRequest) ProvenanceResult {
	f.calls++
	return f.res
}

// A task whose origin cannot be read counts as companion-delegated: the
// lookup error never grants a role's authority.
func TestPipeline_CompanionDelegatedFailsClosed(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	p := NewPipeline(nil, PipelineConfig{Metrics: m, Logger: zerolog.Nop(),
		CompanionDelegated: func(context.Context, string) (bool, error) { return false, errors.New("db down") }})
	if !p.companionDelegated(context.Background(), "proj", "task", "reviewer") {
		t.Fatal("a failed origin lookup was treated as not delegated")
	}
	if got := testutil.ToFloat64(m.PipelineProvenanceChecksTotal.WithLabelValues("proj", "origin_unknown")); got != 1 {
		t.Fatalf("origin_unknown counted %v, want 1", got)
	}
	// A companion producer is capped elsewhere and not asked.
	asked := false
	p = NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop(),
		CompanionDelegated: func(context.Context, string) (bool, error) { asked = true; return true, nil }})
	if p.companionDelegated(context.Background(), "proj", "task", "companion:claude-code") || asked {
		t.Fatal("a companion producer went through the task lookup")
	}
	// Unwired: the origin cannot be read, so the task counts as delegated
	// (round-3 review B1). Before, an unwired pipeline granted role-of-record.
	if !NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop()}).companionDelegated(context.Background(), "p", "t", "reviewer") {
		t.Fatal("an unwired hook granted a role's authority")
	}
}

// verifyProvenance: an output is never eligible; a refused verification
// records its reason and stamps nothing; a record write failure stamps
// nothing; a verification writes the pending record before the stamp.
func TestPipeline_VerifyProvenanceBranches(t *testing.T) {
	ctx := context.Background()
	var recs []*persistence.ChunkProvenance
	record := func(_ context.Context, r *persistence.ChunkProvenance) error {
		cp := *r
		recs = append(recs, &cp)
		return nil
	}
	fp := &fakeProvenance{res: ProvenanceResult{State: ProvenanceUnverified, Reason: ProvReasonBlobMismatch, Path: ProvenanceLLDDir + "/a.md", CheckedAt: time.Now()}}
	p := NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop(), Provenance: fp, RecordProvenance: record})

	if p.verifyProvenance(ctx, "p", "a1", "review.md", "x", IngestArtifactOptions{}) || fp.calls != 0 || len(recs) != 0 {
		t.Fatal("a non-document output was verified or recorded")
	}
	if p.verifyProvenance(ctx, "p", "a2", ProvenanceLLDDir+"/a.md", "x", IngestArtifactOptions{Document: true, RepoScope: "s"}) {
		t.Fatal("a refused verification stamped the chunks")
	}
	if len(recs) != 1 || recs[0].State != ProvenanceUnverified || recs[0].Reason != ProvReasonBlobMismatch || recs[0].ArtifactID != "a2" {
		t.Fatalf("refusal record = %+v", recs)
	}

	failing := NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop(), Provenance: &fakeProvenance{res: ProvenanceResult{State: ProvenanceVerified, CheckedAt: time.Now()}},
		RecordProvenance: func(context.Context, *persistence.ChunkProvenance) error { return errors.New("write failed") }})
	if failing.verifyProvenance(ctx, "p", "a3", ProvenanceLLDDir+"/a.md", "x", IngestArtifactOptions{Document: true}) {
		t.Fatal("chunks were stamped without a written record")
	}

	if NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop()}).verifyProvenance(ctx, "p", "a4", ProvenanceLLDDir+"/a.md", "x", IngestArtifactOptions{Document: true}) {
		t.Fatal("an unwired verifier verified")
	}
}

// The sweep against a real clone: a verified record whose design changes on
// main goes stale and its provenance stamps are withdrawn, in one audited
// event; an unchanged record only moves checked_at; a terminal record is left.
func TestProvenanceSweep_StaleWithdrawsStamps(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	res := f.verify(t, provLLD, provLLDBody)
	if !res.Verified() {
		t.Fatalf("fixture: %+v", res)
	}
	store := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-ok":   ProvenanceRecord(f.project, "art-ok", f.scope, res),
		"art-dead": {ProjectID: f.project, ArtifactID: "art-dead", RepoScope: f.scope, DocumentPath: "docs/plans/p.md", State: ProvenanceUnverified, Reason: ProvReasonPathNotAllowed},
	}}
	s := &ProvenanceSweeper{Store: store, Verifier: f.v, Logger: zerolog.Nop()}

	out, err := s.SweepProject(ctx, f.project)
	if err != nil || out.Examined != 2 || out.Unchanged != 1 || out.Terminal != 1 || out.Changed != 0 || len(store.events) != 0 {
		t.Fatalf("quiet sweep: %+v %v events=%d", out, err, len(store.events))
	}

	f.upstreamCommit(t, provLLD, "# Design 22\n\nChanged on main.\n")
	out, err = s.SweepProject(ctx, f.project)
	if err != nil || out.Changed != 1 || len(store.events) != 1 {
		t.Fatalf("stale sweep: %+v %v events=%d", out, err, len(store.events))
	}
	ev, ch := store.events[0], store.changes[0]
	if ev.Kind != persistence.MemoryVerificationKindSweep || ev.Actor != ProvenanceSweepActor || ev.Examined != 2 {
		t.Fatalf("event: %+v", ev)
	}
	if len(ch) != 1 || ch[0].ArtifactID != "art-ok" || ch[0].NewStatus != "unverified" ||
		ch[0].OnlyValidatorRole != ValidatorRoleProvenance || ch[0].Provenance.State != ProvenanceStale {
		t.Fatalf("change: %+v", ch)
	}
}

// A crash between the pending record and the stamp leaves a pending record
// with no stamp; the next sweep verifies it and stamps its chunks (design
// review c96d finding 1: pending_stamp must be re-tried, not stranded).
func TestProvenanceSweep_PendingRecordIsRepaired(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	res := f.verify(t, provLLD, provLLDBody)
	if !res.Verified() {
		t.Fatalf("fixture: %+v", res)
	}
	pending := ProvenanceRecord(f.project, "art-crashed", f.scope, res)
	pending.State, pending.Reason, pending.VerifiedAt = ProvenanceUnverified, ProvReasonPendingStamp, nil
	if ProvenanceTerminal(pending.State, pending.Reason) {
		t.Fatal("pending_stamp is terminal: a crashed ingest would stay unverified forever")
	}
	store := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{"art-ok": pending}}
	pending.ArtifactID = "art-ok" // the fake lists art-ok and art-dead
	s := &ProvenanceSweeper{Store: store, Verifier: f.v, Logger: zerolog.Nop()}
	out, err := s.SweepProject(ctx, f.project)
	if err != nil || out.Changed != 1 || len(store.changes) != 1 {
		t.Fatalf("repair sweep: %+v %v", out, err)
	}
	ch := store.changes[0][0]
	if ch.NewStatus != "verified" || ch.NewValidatorRole != ValidatorRoleProvenance || ch.Provenance.State != ProvenanceVerified || ch.Provenance.VerifiedAt == nil {
		t.Fatalf("repair change: %+v %+v", ch, ch.Provenance)
	}
}

type fakeSweepStore struct {
	recs    map[string]*persistence.ChunkProvenance
	events  []*persistence.MemoryVerificationEvent
	changes [][]persistence.ArtifactVerificationChange
}

func (s *fakeSweepStore) ListProvenanceProjects(context.Context) ([]string, error) { return nil, nil }
func (s *fakeSweepStore) ListProvenance(_ context.Context, _ string, after string, _ int) ([]*persistence.ChunkProvenance, error) {
	if after != "" {
		return nil, nil
	}
	keys := make([]string, 0, len(s.recs))
	for k := range s.recs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*persistence.ChunkProvenance
	for _, k := range keys {
		cp := *s.recs[k]
		out = append(out, &cp)
	}
	return out, nil
}
func (s *fakeSweepStore) UpsertProvenance(_ context.Context, p *persistence.ChunkProvenance) error {
	cp := *p
	s.recs[p.ArtifactID] = &cp
	return nil
}
func (s *fakeSweepStore) ApplyVerificationChange(_ context.Context, ev *persistence.MemoryVerificationEvent, ch []persistence.ArtifactVerificationChange) error {
	s.events = append(s.events, ev)
	s.changes = append(s.changes, ch)
	for _, c := range ch {
		if c.Provenance != nil {
			cp := *c.Provenance
			s.recs[c.ArtifactID] = &cp
		}
	}
	return nil
}
