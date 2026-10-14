package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"
)

// GitHub #302 (2026-10-13): three Hermes projects sat paused behind DRIFT
// rows on 2026-10-08 and the broker view of their tasks said only QUEUED,
// so the assistant reported a silent hang (config-apply journal design
// §13.1). status, result, task_explain and doctor now say the project is
// paused, by which journal row, and who resolves it (§13.2), in closed
// fields only.

type fakePauseApplier struct {
	fakeApplier
	reasons []controlplane.PauseReason
	err     error
	calls   int
}

func (f *fakePauseApplier) PauseReasons(context.Context) ([]controlplane.PauseReason, error) {
	f.calls++
	return f.reasons, f.err
}

const pauseJournalID = "caj_20261007095821_9a3a0ecb96fb1717"

var pauseSince = time.Date(2026, 10, 7, 9, 58, 21, 0, time.UTC)

func pausedFixture(t *testing.T, projects ...string) (*diagFixture, *fakePauseApplier) {
	t.Helper()
	f := newDiagFixture(t)
	fa := &fakePauseApplier{reasons: []controlplane.PauseReason{{
		JournalID: pauseJournalID, Since: pauseSince, Projects: projects,
		Drift: controlplane.DriftEditedFile, Files: 2,
	}}}
	f.srv.proposalApplier = fa
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	return f, fa
}

// the paused entry every view must carry, exactly.
func wantPaused() map[string]any {
	return map[string]any{
		"reason":      "config_apply_unresolved",
		"journal_id":  pauseJournalID,
		"since":       "2026-10-07T09:58:21Z",
		"drift":       controlplane.DriftEditedFile,
		"files":       float64(2),
		"resolved_by": "operator",
		"resolve":     strings.ReplaceAll(configPauseResolveTemplate, "<id>", pauseJournalID),
	}
}

func pausedOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list, ok := out["paused"].([]any)
	require.True(t, ok, "no paused list in %v", out)
	require.Len(t, list, 1)
	return list[0].(map[string]any)
}

// the forbidden content: the failure text, a path, a hash, the proposal id.
func assertNoPauseLeak(t *testing.T, text string) {
	t.Helper()
	for _, s := range []string{"drift: ", "projects/", ".yaml", "sha256", "prop-"} {
		assert.NotContains(t, text, s)
	}
}

func TestPaused_StatusResultExplain_BrokerAndOrdinaryViewsAgree(t *testing.T) {
	f, _ := pausedFixture(t, "hermes--fin", "alpha")
	f.tasks["t-fin"] = &persistence.Task{ID: "t-fin", ProjectID: "hermes--fin", Status: persistence.TaskStatusQueued}
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusQueued,
		LastError: strp("waiting: configuration apply unresolved (journal " + pauseJournalID + ", DRIFT) for alpha")}

	for _, c := range []struct{ name, raw, task string }{
		{"broker", f.adminRaw, "t-fin"},
		{"ordinary", f.plainRaw, "t-a"},
	} {
		for _, tool := range []string{"status", "result", "task_explain"} {
			out, text, isErr := f.call(t, c.raw, tool, map[string]any{"task_id": c.task})
			require.False(t, isErr, "%s %s: %s", c.name, tool, text)
			assert.Equal(t, wantPaused(), pausedOf(t, out), "%s %s", c.name, tool)
			if c.name == "broker" {
				assertNoPauseLeak(t, text)
			}
		}
	}
}

func TestPaused_AbsentForTerminalTasksAndOtherProjects(t *testing.T) {
	f, _ := pausedFixture(t, "hermes--home")
	f.tasks["t-done"] = &persistence.Task{ID: "t-done", ProjectID: "hermes--home", Status: persistence.TaskStatusCompleted}
	f.tasks["t-fin"] = &persistence.Task{ID: "t-fin", ProjectID: "hermes--fin", Status: persistence.TaskStatusQueued}
	for _, id := range []string{"t-done", "t-fin"} {
		for _, tool := range []string{"status", "task_explain"} {
			out, text, isErr := f.call(t, f.adminRaw, tool, map[string]any{"task_id": id})
			require.False(t, isErr, text)
			assert.NotContains(t, out, "paused", "%s %s", id, tool)
		}
	}
}

func TestPaused_StarPausesEveryProject(t *testing.T) {
	f, _ := pausedFixture(t, "*")
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusQueued}
	out, text, isErr := f.call(t, f.plainRaw, "status", map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.Equal(t, wantPaused(), pausedOf(t, out))
}

// review 0a10 F1: one read fault, seen by both tools — status omits paused
// (the declared residual) and doctor says the state is unknown.
func TestPaused_ReadFaultOmitsInStatusAndFlagsInDoctor(t *testing.T) {
	f, fa := pausedFixture(t, "hermes--fin")
	fa.err = errors.New("db down")
	f.tasks["t-fin"] = &persistence.Task{ID: "t-fin", ProjectID: "hermes--fin", Status: persistence.TaskStatusQueued}
	out, text, isErr := f.call(t, f.adminRaw, "status", map[string]any{"task_id": "t-fin"})
	require.False(t, isErr, text)
	assert.NotContains(t, out, "paused")
	doc, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.Equal(t, true, doc["paused_unavailable"])
	assert.Empty(t, doc["paused_projects"])
	assert.NotContains(t, text, "db down")
}

func TestPaused_DoctorListsOnlyCoveredProjects(t *testing.T) {
	f, _ := pausedFixture(t, "other--x", "hermes--fin", "hermes--home")
	doc, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "other--x")
	assert.Equal(t, false, doc["paused_unavailable"])
	list, ok := doc["paused_projects"].([]any)
	require.True(t, ok, text)
	require.Len(t, list, 2)
	first := list[0].(map[string]any)
	assert.Equal(t, map[string]any{"project": "hermes--fin", "journal_id": pauseJournalID,
		"since": "2026-10-07T09:58:21Z", "drift": controlplane.DriftEditedFile, "files": float64(2)}, first)
	assert.Equal(t, "hermes--home", list[1].(map[string]any)["project"])
	assertNoPauseLeak(t, text)
}

// With no journal reader wired nothing can be paused: doctor reports none
// and no fault.
func TestPaused_DoctorWithoutAJournalIsCleanNotUnavailable(t *testing.T) {
	f := newDiagFixture(t)
	doc, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.Equal(t, false, doc["paused_unavailable"])
	assert.Empty(t, doc["paused_projects"])
}

// An agent learns of `paused` from the tool descriptions it reads (§13.2).
func TestPaused_ToolDescriptionsNameIt(t *testing.T) {
	want := map[string]string{"status": "paused", "result": "paused", "task_explain": "paused", "doctor": "paused_projects"}
	seen := 0
	for _, d := range companionToolDefs() {
		if w, ok := want[d.Name]; ok {
			seen++
			assert.Contains(t, d.Description, w, d.Name)
		}
	}
	assert.Equal(t, len(want), seen, "examined tools")
}

// Once the device card ships (§13.3), the sentence names the device and the
// host for every key, and never depends on whether a device is paired.
func TestPaused_ResolveSentenceNamesTheDeviceAndTheHost(t *testing.T) {
	for _, want := range []string{"approver device", "A config change was interrupted", "vornikctl control-plane journal resolve <id>", "You cannot resolve this yourself"} {
		assert.Contains(t, configPauseResolveTemplate, want)
	}
}

// The agent can read the pause and nothing more: no companion tool resolves
// a journal row (§13.2 "What the agent still cannot do").
func TestPaused_NoCompanionToolResolves(t *testing.T) {
	for _, def := range companionToolDefs() {
		name := strings.ToLower(def.Name)
		assert.False(t, strings.Contains(name, "resolve") || strings.Contains(name, "journal"),
			"tool %s must not reach the journal resolve verb", def.Name)
	}
}
