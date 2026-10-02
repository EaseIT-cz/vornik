package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/brokerschedule"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

const scheduledWorkflowMD = `---
workflowId: hermes--fin--digest
entrypoint: read
broker:
  input_schema:
    type: object
    additionalProperties: false
    required: [month]
    properties:
      month: { enum: [current, previous] }
  egress:
    output: result.json
    schema: { type: object, additionalProperties: false, properties: { total: { type: number } } }
  schedule: {"cron":"0 8 1 * *","timezone":"Europe/Prague","inputs":{"month":"%s"}}
steps:
  read:
    type: agent
    prompt: "Summarise the month."
    role: reader
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`

// seedScheduledRegistry loads an agent namespace with one scheduled broker
// workflow whose fixed input is month.
func seedScheduledRegistry(t *testing.T, month string) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	write := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}
	write("swarms/hermes--fin.md", "---\nswarmId: hermes--fin\nroles:\n  - name: reader\n    runtime:\n      image: test-image\n    permissions:\n      allowedTools: [file_write]\n---\n")
	write("projects/hermes--fin.yaml", "projectId: hermes--fin\ndisplayName: Fin\nswarmId: hermes--fin\ndefaultWorkflowId: hermes--fin--digest\ndefaultPriority: 50\nbroker: true\n")
	write("projects/hermes--home.yaml", "projectId: hermes--home\ndisplayName: Home\nswarmId: hermes--fin\ndefaultWorkflowId: hermes--fin--digest\ndefaultPriority: 50\nbroker: true\n")
	write("workflows/hermes--fin--digest.md", strings.Replace(scheduledWorkflowMD, "%s", month, 1))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	require.NotNil(t, reg.GetWorkflow("hermes--fin--digest"), "the scheduled workflow did not load")
	return reg
}

func newScheduleServer(t *testing.T, reg *registry.Registry) (*Server, *memAPIKeyRepo, *mocks.MockTaskRepository) {
	t.Helper()
	keys := &memAPIKeyRepo{}
	tasks := &mocks.MockTaskRepository{}
	return &Server{logger: zerolog.Nop(), apiKeyRepo: keys, taskRepo: tasks, projectRegistry: reg,
		taskCreator: taskcreate.New(taskcreate.WithTaskRepository(tasks), taskcreate.WithProjectRegistry(reg))}, keys, tasks
}

// Agent-administered Vornik design §17.3, plan P7.2: a scheduled fire takes
// the delegate path: the same prompt and payload shape, the approved fixed
// inputs, source SCHEDULED, the slot's idempotency key, attributed to the
// namespace's live agent admin key. Control: FireScheduledBroker through
// createBrokerTask.
func TestFireScheduledBroker(t *testing.T) {
	srv, keys, tasks := newScheduleServer(t, seedScheduledRegistry(t, "previous"))
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{ID: "akey-h", ProjectID: "hermes--home", Name: "hermes",
		KeyHash: "h", KeyPrefix: "sk-vornik-hermes", ClientKind: "hermes", CreatedAt: time.Now(), AgentAdmin: true, AgentNamespace: "hermes"}))

	id, err := srv.FireScheduledBroker(context.Background(), "hermes--fin--digest", "sched:hermes--fin--digest:2026-11-01T08:00")
	require.NoError(t, err)
	require.NotEmpty(t, id)
	task := tasks.LastCall.Task
	require.Equal(t, persistence.TaskCreationSourceScheduled, task.CreationSource)
	require.Equal(t, "hermes--fin", task.ProjectID)
	require.NotNil(t, task.IdempotencyKey)
	require.Equal(t, "sched:hermes--fin--digest:2026-11-01T08:00", *task.IdempotencyKey)
	require.NotNil(t, task.CreatedByAPIKeyID)
	require.Equal(t, "akey-h", *task.CreatedByAPIKeyID)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(task.Payload, &payload))
	ctx := payload["context"].(map[string]any)
	require.Equal(t, map[string]any{"month": "previous"}, ctx["broker_inputs"])
	require.Contains(t, ctx["prompt"], "previous")
	require.Equal(t, map[string]any{"workflow": "hermes--fin--digest"}, ctx["broker"])
}

// With no live key (the assistant was disconnected) the approved schedule
// still runs, attributed to no key (design §17.3).
func TestFireScheduledBroker_NoLiveKey(t *testing.T) {
	srv, _, tasks := newScheduleServer(t, seedScheduledRegistry(t, "previous"))
	_, err := srv.FireScheduledBroker(context.Background(), "hermes--fin--digest", "sched:x")
	require.NoError(t, err)
	require.Nil(t, tasks.LastCall.Task.CreatedByAPIKeyID)
}

// Review 6a3f R1/R3: approved inputs re-validated against the live schema
// refuse the fire (ErrInputs) and create nothing; a workflow that vanished,
// or one that has no schedule, is an error of its own kind.
func TestFireScheduledBroker_Refusals(t *testing.T) {
	// The loader refuses such inputs, so the defence is reached by changing
	// the loaded object, as a schema narrowed under an approved schedule.
	reg := seedScheduledRegistry(t, "previous")
	reg.GetWorkflow("hermes--fin--digest").Broker.Schedule.Inputs["month"] = "someday"
	srv, _, tasks := newScheduleServer(t, reg)
	_, err := srv.FireScheduledBroker(context.Background(), "hermes--fin--digest", "sched:x")
	require.True(t, errors.Is(err, brokerschedule.ErrInputs), "%v", err)
	require.Equal(t, 0, tasks.CallCount.Create)

	_, err = srv.FireScheduledBroker(context.Background(), "hermes--fin--gone", "sched:x")
	require.Error(t, err)
	require.False(t, errors.Is(err, brokerschedule.ErrInputs))

	broker, _, _ := newScheduleServer(t, seedBrokerRegistry(t))
	_, err = broker.FireScheduledBroker(context.Background(), "mail-digest", "sched:x")
	require.Error(t, err, "an operator workflow, which has no schedule, was fired")
}
