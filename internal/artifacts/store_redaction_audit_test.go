package artifacts

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// Secret-leak Phase 3 follow-up (design 2026-07-11, "the remaining sinks",
// 2026-10-01): the artifact checkpoint redacted and logged but recorded
// nothing to secret_redaction_audit, so the task badge and scan history never
// saw an artifact finding.

type fakeRedactionRecorder struct {
	mu     sync.Mutex
	events []persistence.SecretRedactionEvent
	err    error
}

func (f *fakeRedactionRecorder) Record(_ context.Context, ev []persistence.SecretRedactionEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev...)
	return f.err
}

func TestStore_RecordsStrongArtifactFindings(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)
	rec := &fakeRedactionRecorder{}
	store.SetRedactionRecorder(rec)

	src := writeTempSource(t, "result.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv\n")
	_, err := store.Store(context.Background(), "p1", "exec1", "task1", "result.md", src)
	require.NoError(t, err)

	require.Len(t, rec.events, 1)
	ev := rec.events[0]
	assert.Equal(t, "p1", ev.ProjectID)
	assert.Equal(t, "task1", ev.TaskID)
	assert.Equal(t, secrets.CheckpointArtifacts, ev.Checkpoint)
	assert.Equal(t, "openai_key", ev.FindingType)
	assert.Equal(t, 1, ev.Count)
	assert.Equal(t, "live", ev.Source)
}

// Detect mode still records: the audit is what the scan found.
func TestStore_DetectModeStillRecords(t *testing.T) {
	store := newTestStoreWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointArtifacts: secrets.ActionDetect,
	})
	rec := &fakeRedactionRecorder{}
	store.SetRedactionRecorder(rec)
	src := writeTempSource(t, "result.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv\n")
	_, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)
	require.Len(t, rec.events, 1)
}

// Heuristic-only findings (entropy, generic_kv) are not recorded on this
// surface: ~7,000 of them with zero true positives were measured on stored
// artifacts (artifact heuristic-redaction design).
func TestStore_HeuristicOnlyFindingsAreNotRecorded(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)
	rec := &fakeRedactionRecorder{}
	store.SetRedactionRecorder(rec)
	body := "password = \"hunter2hunter2hunter2\"\n"

	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	findings := det.Scan([]byte(body))
	// A fixture that stops producing heuristic-only findings must fail, not
	// skip: a skipped test proves nothing.
	require.NotEmpty(t, findings, "the fixture must produce a finding")
	require.Empty(t, secrets.DropHeuristic(findings), "the fixture's findings must all be heuristic")

	src := writeTempSource(t, "result.md", body)
	_, err = store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)
	assert.Empty(t, rec.events)
}

// A failed record never fails the store.
func TestStore_RecordFailureDoesNotFailTheWrite(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)
	store.SetRedactionRecorder(&fakeRedactionRecorder{err: errors.New("db down")})
	src := writeTempSource(t, "result.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv\n")
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)
	require.NotNil(t, art)
}
