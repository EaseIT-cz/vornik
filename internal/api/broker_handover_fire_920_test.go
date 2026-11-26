package api

// GitHub #920 (agent-administered design part 24 §24.3 B2-B3): the daemon
// side of hand_over, through createBrokerTask with the real registry and task
// creator.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

func seedHandOverRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	write := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}
	swarm := "---\nswarmId: hermes--s\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n    permissions:\n      allowedTools: [file_write]\n---\n"
	write("swarms/hermes--s.md", swarm)
	for p, wf := range map[string]string{"research": "hermes--research--scout", "venues": "hermes--venues--enrich", "home": "hermes--research--scout"} {
		write("projects/hermes--"+p+".yaml", "projectId: hermes--"+p+"\ndisplayName: "+p+"\nswarmId: hermes--s\ndefaultWorkflowId: "+wf+"\ndefaultPriority: 50\nbroker: true\n")
	}
	step := "steps:\n  work:\n    type: agent\n    prompt: \"Work.\"\n    role: worker\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"
	egress := "  egress:\n    output: result.json\n    schema: { type: object, additionalProperties: false, properties: { n: { type: integer } } }\n"
	write("workflows/hermes--research--scout.md", "---\nworkflowId: hermes--research--scout\nentrypoint: work\nbroker:\n"+
		"  input_schema: { type: object, additionalProperties: false, properties: {} }\n"+egress+
		"  hand_over: {\"workflow\":\"hermes--venues--enrich\",\"documents\":{\"venues\":\"artifacts/out/scout.md\"}}\n"+step)
	write("workflows/hermes--venues--enrich.md", "---\nworkflowId: hermes--venues--enrich\nentrypoint: work\nbroker:\n"+
		"  input_schema: { type: object, additionalProperties: false, required: [venues], properties: { venues: { type: string, x-untrusted-document: { max_bytes: 65536, media_type: text/markdown } } } }\n"+egress+step)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	require.NotNil(t, reg.GetWorkflow("hermes--research--scout"))
	require.NotNil(t, reg.GetWorkflow("hermes--venues--enrich"))
	return reg
}

type handOverRig struct {
	srv    *Server
	tasks  *mocks.MockTaskRepository
	src    *persistence.Task
	exec   *persistence.Execution
	wf     *registry.Workflow
	output string // the scout.md body; "" for none
}

func newHandOverRig(t *testing.T) *handOverRig {
	t.Helper()
	reg := seedHandOverRegistry(t)
	srv, keys, tasks := newScheduleServer(t, reg)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{ID: "akey-h", ProjectID: "hermes--home", Name: "hermes",
		KeyHash: "h", KeyPrefix: "sk-vornik-hermes", ClientKind: "hermes", CreatedAt: time.Now(), AgentAdmin: true, AgentNamespace: "hermes"}))
	rig := &handOverRig{srv: srv, tasks: tasks, wf: reg.GetWorkflow("hermes--research--scout"),
		src:  &persistence.Task{ID: "task_scout", ProjectID: "hermes--research", Status: persistence.TaskStatusCompleted, Payload: []byte(`{"context":{}}`)},
		exec: &persistence.Execution{ID: "exec_scout", WorkflowID: "hermes--research--scout"}, output: "# 30 venues\n"}
	dir := t.TempDir()
	ar := &mocks.MockArtifactRepository{}
	ar.ListFunc = func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		if rig.output == "" || f.ExecutionID == nil || *f.ExecutionID != rig.exec.ID {
			return nil, nil
		}
		p := filepath.Join(dir, "scout.md")
		require.NoError(t, os.WriteFile(p, []byte(rig.output), 0o600))
		return []*persistence.Artifact{{ID: "a1", ProjectID: "hermes--research", Name: "scout-20261125-a8f2.md", // the harvested shape (#926)
			ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p, CreatedAt: time.Now()}}, nil
	}
	srv.artifactRepo = ar
	return rig
}

func (r *handOverRig) fire() (string, error) {
	return r.srv.FireHandover(context.Background(), r.src, r.exec, r.wf)
}

func TestFireHandover_StartsTheTargetWithTheDocument_920(t *testing.T) {
	rig := newHandOverRig(t)
	id, err := rig.fire()
	require.NoError(t, err)
	require.NotEmpty(t, id)
	task := rig.tasks.LastCall.Task
	require.Equal(t, "hermes--venues", task.ProjectID)
	require.Equal(t, persistence.TaskCreationSourceHandover, task.CreationSource)
	require.NotNil(t, task.IdempotencyKey)
	require.Equal(t, "handover:task_scout:hermes--venues--enrich", *task.IdempotencyKey)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(task.Payload, &payload))
	ctx := payload["context"].(map[string]any)
	require.Equal(t, "# 30 venues\n", ctx["broker_inputs"].(map[string]any)["venues"])
	require.Equal(t, map[string]any{"project": "hermes--research", "workflow": "hermes--research--scout", "task": "task_scout"}, ctx["handed_over_from"])
	require.EqualValues(t, 1, ctx["handover_depth"])
	// The document is staged as a file, never inlined into the prompt.
	require.NotContains(t, ctx["prompt"], "30 venues")
}

func TestFireHandover_Refusals_920(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*handOverRig)
		reason string
	}{
		"missing document": {func(r *handOverRig) { r.output = "" }, "missing_document"},
		"depth":            {func(r *handOverRig) { r.src.Payload = []byte(`{"context":{"handover_depth":3}}`) }, "depth"},
		"oversized":        {func(r *handOverRig) { r.output = strings.Repeat("x", 70000) }, "inputs"},
		"target gone": {func(r *handOverRig) {
			h := *r.wf.Broker.HandOver
			h.Workflow = "hermes--venues--missing"
			b := *r.wf.Broker
			b.HandOver = &h
			w := *r.wf
			w.Broker = &b
			r.wf = &w
		}, "not_delegable"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newHandOverRig(t)
			tc.mutate(rig)
			_, err := rig.fire()
			var refused *executor.HandOverRefusal
			require.True(t, errors.As(err, &refused), "want a refusal, got %v", err)
			require.Equal(t, tc.reason, refused.Reason)
			require.Nil(t, rig.tasks.LastCall.Task, "a refused hand-over creates no task")
		})
	}
}

// review-20261125-7024 suggestion 6: the namespace key's companion budget is
// checked, as for a scheduled run.
func TestFireHandover_BudgetRefused_920(t *testing.T) {
	rig := newHandOverRig(t)
	repo := rig.srv.apiKeyRepo.(*memAPIKeyRepo)
	require.Len(t, repo.rows, 1)
	limit := 1.0
	repo.rows[0].BudgetCapUSD = &limit
	rig.srv.llmUsageRepo = spendStub{spend: 2}
	_, err := rig.fire()
	var refused *executor.HandOverRefusal
	require.True(t, errors.As(err, &refused), "want a refusal, got %v", err)
	require.Equal(t, "budget", refused.Reason)
	require.Nil(t, rig.tasks.LastCall.Task)
}

// review-20261125-23bb F2: the mapped file is found past the first page of
// the execution's artifacts.
func TestFireHandover_FindsTheDocumentPastThePage_920(t *testing.T) {
	rig := newHandOverRig(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "scout.md")
	require.NoError(t, os.WriteFile(p, []byte("# late venues\n"), 0o600))
	ar := &mocks.MockArtifactRepository{}
	ar.ListFunc = func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		if f.Offset == 0 { // a full first page of other files
			out := make([]*persistence.Artifact, handOverPage)
			for i := range out {
				out[i] = &persistence.Artifact{ID: "x", ProjectID: "hermes--research", Name: "other.md", ArtifactClass: persistence.ArtifactClassOutput}
			}
			return out, nil
		}
		return []*persistence.Artifact{{ID: "a2", ProjectID: "hermes--research", Name: "scout-20261125-a8f2.md", // the harvested shape (#926)
			ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p, CreatedAt: time.Now()}}, nil
	}
	rig.srv.artifactRepo = ar
	_, err := rig.fire()
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rig.tasks.LastCall.Task.Payload, &payload))
	require.Equal(t, "# late venues\n", payload["context"].(map[string]any)["broker_inputs"].(map[string]any)["venues"])
}
