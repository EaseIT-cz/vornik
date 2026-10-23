package auditredact_test

// GitHub #425 (chat-audit retention and redaction design §11.2.1): the
// step-outcome decorator's bypass paths count every Record as skipped, with
// the reason, and still store the row. Code review 174a F4.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secrets"
)

func TestStepOutcomes425_BypassesCountSkippedWithReason(t *testing.T) {
	nop := zerolog.Nop()
	for _, tc := range []struct {
		name   string
		build  func(auditredact.StepOutcomeStore) *auditredact.StepOutcomes
		reason auditredact.Reason
	}{
		{"nil detector", func(in auditredact.StepOutcomeStore) *auditredact.StepOutcomes {
			return auditredact.NewStepOutcomes(in, nil, nil, &nop)
		}, auditredact.ReasonDetectorNil},
		{"secrets disabled", func(in auditredact.StepOutcomeStore) *auditredact.StepOutcomes {
			return auditredact.NewStepOutcomesBypassed(in, auditredact.ReasonSecretsDisabled, &nop)
		}, auditredact.ReasonSecretsDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sqlitetest.Memory(t)
			so := tc.build(sqlite.NewExecutionStepOutcomeRepository(db.DB))
			// A nil detector is not a bypass the container chose; it reports none.
			if want := tc.reason; want != auditredact.ReasonDetectorNil && so.BypassReason() != want {
				t.Fatalf("BypassReason = %q, want %q", so.BypassReason(), want)
			}
			m := auditredact.NewMetrics()
			so.SetMetrics(m)
			o := &persistence.ExecutionStepOutcome{ID: "o1", ProjectID: "p", TaskID: "t", ExecutionID: "e",
				StepID: "s", Role: "r", Model: "m", Outcome: "ok", HallucinationSignals: []byte(`[{"claim_value":"x"}]`)}
			if err := so.Record(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if got := m.SeamTotals()[auditredact.StoreExecutionStepOutcomes]; got.Skipped != 1 || got.Total != 1 {
				t.Fatalf("a bypassed Record must count as skipped: %+v", got)
			}
			if o.RecordedAt.IsZero() {
				t.Fatal("the repository's RecordedAt must reach the caller")
			}
		})
	}
}

// The scanning path copies the repository-assigned RecordedAt back too.
func TestStepOutcomes425_ScanningCopiesRecordedAtBack(t *testing.T) {
	db := sqlitetest.Memory(t)
	d, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.EffectivePatterns(nil, nil), Allowlist: secrets.DefaultAllowlist()})
	if err != nil {
		t.Fatal(err)
	}
	so := auditredact.NewStepOutcomes(sqlite.NewExecutionStepOutcomeRepository(db.DB), d, nil, nil)
	o := &persistence.ExecutionStepOutcome{ID: "o2", ProjectID: "p", TaskID: "t", ExecutionID: "e",
		StepID: "s", Role: "r", Model: "m", Outcome: "ok"}
	if err := so.Record(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if o.RecordedAt.IsZero() {
		t.Fatal("the decorator dropped the RecordedAt the repository assigned")
	}
}
