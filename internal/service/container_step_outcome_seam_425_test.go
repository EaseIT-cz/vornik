package service

// GitHub #425 — execution_step_outcomes.hallucination_signals had no write-time
// secret seam (chat-audit retention and redaction design §11.2.1). Found at:
// review (#414's §10.2.5). This is the wiring half: the decorator is installed
// in every branch, counted, and — because three callers type-assert the
// outcome repository for optional methods — it must not hide them. The stored
// row is checked at integration level by repotest.RunStepOutcomeSeamSuite on
// both drivers.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/watchdog"
)

// Both production backends must satisfy the store the decorator wraps;
// otherwise the container leaves the store undecorated (design §11.2.1).
var (
	_ auditredact.StepOutcomeStore = (*sqlite.ExecutionStepOutcomeRepository)(nil)
	_ auditredact.StepOutcomeStore = (*postgres.ExecutionStepOutcomeRepository)(nil)
)

func stepOutcomeSeamContainer(t *testing.T, enabled bool) *Container {
	t.Helper()
	c := contentSeamContainer(t, enabled)
	c.repos.StepOutcomes = sqlite.NewExecutionStepOutcomeRepository(sqlitetest.Memory(t).DB)
	return c
}

// The capability assertions the scheduler and the executor make.
func requireOutcomeCapabilities(t *testing.T, repo persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	_, ok := repo.(watchdog.StepOutcomeRepository)
	require.True(t, ok, "the decorator hid SweepPendingForTerminalExecutions")
	_, ok = repo.(watchdog.StuckStepRecorder)
	require.True(t, ok, "the decorator hid RecordStuckStepOutcome")
	_, ok = repo.(interface {
		SweepPendingForTaskOrphans(ctx context.Context, taskID string) (int64, error)
	})
	require.True(t, ok, "the decorator hid SweepPendingForTaskOrphans")
}

func TestStepOutcomeSeam425_DecoratedCountedAndCapable(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		c := stepOutcomeSeamContainer(t, enabled)
		c.initToolAuditRedaction()

		so, ok := c.repos.StepOutcomes.(*auditredact.StepOutcomes)
		require.True(t, ok, "step outcomes left undecorated (enabled=%v): %T", enabled, c.repos.StepOutcomes)
		want := auditredact.ReasonSecretsDisabled
		if enabled {
			want = auditredact.ReasonNone
		}
		require.Equal(t, want, so.BypassReason())
		requireOutcomeCapabilities(t, c.repos.StepOutcomes)

		require.NoError(t, c.repos.StepOutcomes.Record(context.Background(), &persistence.ExecutionStepOutcome{
			ID: "o-" + map[bool]string{true: "on", false: "off"}[enabled], ProjectID: "p", TaskID: "t", ExecutionID: "e",
			StepID: "s", Role: "r", Model: "m", Outcome: "ok",
		}))
		tot := c.auditRedactMetrics.SeamTotals()[auditredact.StoreExecutionStepOutcomes]
		if enabled {
			require.Equal(t, int64(1), tot.Scanned)
		} else {
			require.Equal(t, int64(1), tot.Skipped)
		}

		reason, stores := c.redactionSeamsState()
		require.Equal(t, want, reason)
		require.Contains(t, stores, auditredact.StoreExecutionStepOutcomes)
	}
}

// The rebuild re-runs the wiring: a scanning decorator is kept, a bypass is
// upgraded by wrapping its Inner(), never nested.
func TestStepOutcomeSeam425_IdempotentAndUpgrades(t *testing.T) {
	c := stepOutcomeSeamContainer(t, false)
	c.initToolAuditRedaction()
	first := c.repos.StepOutcomes
	c.initToolAuditRedaction()
	require.Same(t, first, c.repos.StepOutcomes, "a same-reason bypass is kept")

	c.Config.Secrets.Enabled = true
	c.initToolAuditRedaction()
	so := c.repos.StepOutcomes.(*auditredact.StepOutcomes)
	require.Equal(t, auditredact.ReasonNone, so.BypassReason())
	_, nested := so.Inner().(*auditredact.StepOutcomes)
	require.False(t, nested, "the upgrade nested the seam over the bypass")

	scanning := c.repos.StepOutcomes
	c.initToolAuditRedaction()
	require.Same(t, scanning, c.repos.StepOutcomes, "a scanning decorator must not be wrapped again")
}

// A repository without the optional capabilities (a test double) is left
// undecorated rather than given capabilities it does not have; the doctor
// snapshot reports it present and undecorated.
func TestStepOutcomeSeam425_NarrowRepositoryIsReportedNotWrapped(t *testing.T) {
	c := stepOutcomeSeamContainer(t, true)
	narrow := narrowOutcomes{c.repos.StepOutcomes}
	c.repos.StepOutcomes = narrow
	c.initToolAuditRedaction()
	require.Equal(t, narrow, c.repos.StepOutcomes)
	reason, stores := c.redactionSeamsState()
	require.Equal(t, auditredact.ReasonDetectorNil, reason)
	require.Contains(t, stores, auditredact.StoreExecutionStepOutcomes)
}

// narrowOutcomes exposes only the wide interface.
type narrowOutcomes struct {
	persistence.ExecutionStepOutcomeRepository
}
