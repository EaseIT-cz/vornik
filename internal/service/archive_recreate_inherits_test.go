package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/testutil/testwait"
	"vornik.io/vornik/internal/ui"
)

// GitHub #370 carry-over, delta audit 2026-10-14 (lane W8-W, archival design
// section I). Found at: code review; observed when an operator re-creates a
// project with a deleted project's ID. agent_workflow_approvals (keyed by
// workflow_id) and agent_integration_approvals (keyed by project_id +
// integration) were left behind by the project wipe, so a re-created agent
// project ran its workflow and called its integration's tools on the deleted
// project's device approvals. Deletes through the daemon wiring
// (initArchiveSweeper + initArchiveLifecycle on SQLite, the #365 fixture) and
// DeleteNowAndWait, re-creates the project, and asks the code paths that
// decide: verifyAgentReach (task creation, plan resolve, retry) and
// agentadmin.ToolApprovalRefusal (every agent MCP tool call).
func TestArchiveDeleteNow_RecreatedAgentProjectInheritsNoApproval_370(t *testing.T) {
	const (
		project  = "hermes--doomed"
		workflow = "hermes--doomed--wf"
		tool     = "mcp__jira__search"
	)
	ctx := context.Background()
	dir := t.TempDir()
	db := sqlitetest.Open(t, filepath.Join(dir, "vornik.db"))
	configs := filepath.Join(dir, "configs")
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configs, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectYAML := "projectId: " + project + "\ndisplayName: doomed\nswarmId: hermes--s\nbroker: true\ndefaultWorkflowId: " + workflow + "\n"
	archived := projectYAML + "lifecycle:\n  status: archived\n  archivedAt: \"" + time.Now().UTC().Format(time.RFC3339) +
		"\"\n  scheduledDeleteAt: \"" + time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339) + "\"\n"
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(configs, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("swarms/hermes--s.md", "---\nswarmId: hermes--s\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n")
	write("workflows/"+workflow+".md", "---\nworkflowId: "+workflow+"\nentrypoint: run\nbroker:\n  input_schema: {type: object, additionalProperties: false}\n"+
		"  egress:\n    output: \"result.json\"\n    max_bytes: 4096\n    schema: {type: object, additionalProperties: false, properties: {note: {type: string, maxLength: 100}}}\n"+
		"steps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n")
	write("projects/"+project+".yaml", archived)
	reg := registry.New()
	if err := reg.Load(configs); err != nil {
		t.Fatalf("load registry: %v", err)
	}

	grants := sqlite.NewAgentGrantRepository(db.DB)
	toolGrants := sqlite.NewExecutionToolGrantRepository(db.DB)
	p, wf := reg.GetProject(project), reg.GetWorkflow(workflow)
	if p == nil || wf == nil {
		t.Fatalf("fixture not loaded: project %v workflow %v", p != nil, wf != nil)
	}
	sig, err := agentadmin.SignatureOf(p, reg.GetSwarm(p.SwarmID), wf)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := grants.UpsertWorkflowReach(ctx, persistence.AgentWorkflowApproval{Namespace: "hermes", WorkflowID: workflow,
		ProjectID: project, ReachHash: sig.Hash(), ApprovedByDevice: "dev_1", ApprovedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := grants.UpsertIntegration(ctx, persistence.AgentIntegrationApproval{Namespace: "hermes", ProjectID: project,
		Integration: agentns.IntegrationOf("jira"), Kind: "mcp", URL: "https://mcp.example/jira",
		ReadTools: []string{"search"}, WriteTools: []string{}, ApprovedByDevice: "dev_1", ApprovedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := toolGrants.Record(ctx, &persistence.ExecutionToolGrant{ExecutionID: "exec-doomed", ProjectID: project,
		StepID: "run", Role: "worker", RequestedTools: []string{"file_read"}, Accepted: true, RefusedTools: []string{},
		Actor: "lead", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Database.Driver = "sqlite"
	cfg.Storage.ArtifactsPath = filepath.Join(dir, "artifacts")
	c := &Container{Logger: zerolog.Nop(), Config: cfg, DB: db.DB, Registry: reg,
		repos: &storage.Repositories{AgentGrants: grants}}
	if err := c.verifyAgentReach(ctx, project, workflow); err != nil {
		t.Fatalf("precondition: the approved workflow is refused: %v", err)
	}
	if r := agentadmin.ToolApprovalRefusal(ctx, grants, project, tool, false); r != "" {
		t.Fatalf("precondition: the approved tool is refused: %s", r)
	}

	c.archiveSweeper = c.initArchiveSweeper()
	lc := c.initArchiveLifecycle(ui.LifecyclePatcher())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.archiveSweeper.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	res, err := lc.DeleteNowAndWait(ctx, project, true, testwait.ForOutcome(t))
	if err != nil || res.State != projectarchive.DeleteNowDeleted {
		t.Fatalf("delete-now = %q (%v, %v), want deleted", res.State, res.Err, err)
	}

	// The operator re-creates the project under the same ID, same workflow.
	write("projects/"+project+".yaml", projectYAML)
	if err := reg.Load(configs); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if reg.GetProject(project) == nil {
		t.Fatal("the re-created project did not load")
	}
	if err := c.verifyAgentReach(ctx, project, workflow); err == nil || !strings.Contains(err.Error(), "has not been approved") {
		t.Errorf("the re-created project's workflow ran on the deleted project's approval (verifyAgentReach err = %v)", err)
	}
	if r := agentadmin.ToolApprovalRefusal(ctx, grants, project, tool, false); r != "the server is not approved for this project" {
		t.Errorf("the re-created project's tool call was allowed on the deleted project's approval (refusal %q)", r)
	}
	if _, err := toolGrants.Current(ctx, "exec-doomed", "run"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the deleted project's execution_tool_grants row survived (err = %v)", err)
	}
}
