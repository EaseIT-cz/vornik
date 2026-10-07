package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
)

// CA-19's residue: the journal's generation_before/after pair is supposed to be
// evidence that the running registry re-parsed what the apply wrote. It could
// not be, because the marker was a digest over the resolved project set — it
// says WHAT is resolved, never that anything was re-read. An apply whose reload
// silently did nothing produced the same two values as one that worked.
//
// These tests pin the two halves of the closure: the marker now carries a real
// activation counter, and the verification refuses when that counter has not
// moved.

func writeOp(t *testing.T, root, rel, content string) controlplane.JournaledOp {
	t.Helper()
	target := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return controlplane.JournaledOp{
		Path:          rel,
		ContentSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
	}
}

// Issue #66 (2026-10-06): removal unlinked a workflow successfully, then the
// production generation verifier read the missing file and reverted the bundle.
// Exercise the real durable engine/SQLite ledger/registry verification seam.
func TestProposalApplier_DeleteWorkflowCommitsJournal(t *testing.T) {
	c := newProposalApplierContainer(t)
	db := sqlitetest.Memory(t)
	c.repos.Proposals = sqlite.NewProposalRepository(db.DB)
	c.repos.ApplyJournal = sqlite.NewApplyJournalRepository(db.DB)
	c.Registry = registry.New()
	dir := filepath.Dir(c.ConfigPath)
	pid := "hermes--pagedrop-publish"
	wid := pid + "--publish-page"
	projectOp := writeOp(t, dir, "configs/projects/"+pid+".yaml", "old project")
	swarmOp := writeOp(t, dir, "configs/swarms/"+pid+".md", "old swarm")
	// The live registry still references the workflow while its file has
	// already disappeared: precisely the customer report's starting state.
	st := &agentadmin.State{
		Namespace: "hermes",
		Projects: map[string]*agentadmin.ProjectState{pid: {
			ID: pid, DefaultWorkflowID: wid, Swarm: agentadmin.SwarmState{ID: pid},
		}},
		Workflows: map[string]*agentadmin.WorkflowState{wid: {ID: wid, Project: pid}},
		FileHashes: map[string]string{
			"projects/" + pid + ".yaml": projectOp.ContentSHA256,
			"swarms/" + pid + ".md":     swarmOp.ContentSHA256,
		},
	}
	change, err := (&agentadmin.Renderer{}).Render(st, agentadmin.VerbRemove,
		json.RawMessage(`{"kind":"project","id":"pagedrop-publish"}`))
	if err != nil || change.Class != agentadmin.Inert {
		t.Fatalf("remove plan: %+v %v", change, err)
	}
	if len(change.Narrow.RemovedWorkflows) != 1 || change.Narrow.RemovedWorkflows[0] != wid {
		t.Fatalf("stale workflow not removed: %+v", change.Narrow)
	}
	ops := make([]map[string]string, 0, len(change.Ops))
	for _, op := range change.Ops {
		if op.Path == "workflows/"+wid+".md" {
			t.Fatal("missing workflow must not become a conflicting delete op")
		}
		ops = append(ops, map[string]string{"op": op.Op, "path": "configs/" + op.Path})
	}
	engine := c.newProposalApplier()
	engine.LeaderGate = nil // isolated fixture has no running leader worker
	engine.Reload = func() error { return c.Registry.Load(dir) }
	raw, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "removed",
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: "remove workflow", ApplyOps: string(raw), Status: persistence.ProposalStatusDraft,
		ProposedBy: "agent:hermes", LiveApply: true,
	}
	ctx := context.Background()
	if err := c.repos.Proposals.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := c.repos.Proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(ctx, p.ID, "operator", false); err != nil {
		t.Fatalf("delete apply: %v", err)
	}
	for _, path := range []string{projectOp.Path, swarmOp.Path, "configs/workflows/" + wid + ".md"} {
		if _, err := os.Lstat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("deleted target %s still present: %v", path, err)
		}
	}
	got, err := c.repos.Proposals.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal status = %s", got.Status)
	}
	var journalID string
	if err := db.DB.QueryRowContext(ctx, `SELECT id FROM config_apply_journal WHERE proposal_id = ?`, p.ID).Scan(&journalID); err != nil {
		t.Fatal(err)
	}
	row, err := c.repos.ApplyJournal.Get(ctx, journalID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.State != persistence.JournalStateApplied || row.TerminalAt == nil {
		t.Fatalf("journal not durably applied: %+v", row)
	}
}

// Issue #66: deletion verification must accept absence without weakening the
// registry activation gate or accepting a recreated directory entry.
func TestVerifyConfigGeneration_DeletePostState(t *testing.T) {
	for _, state := range []string{"absent", "file", "directory", "symlink", "dangling", "no reload", "legacy marker", "not directory", "escape"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}
			before := c.configGeneration()
			if state == "legacy marker" {
				before = "pre-counter-digest"
			}
			if state != "no reload" {
				activate(t, c.Registry, dir)
			}
			target := filepath.Join(dir, "removed.md")
			path := "removed.md"
			switch state {
			case "file":
				if err := os.WriteFile(target, []byte("recreated"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink", "dangling":
				other := filepath.Join(dir, "other")
				if state == "symlink" {
					if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(other, target); err != nil {
					t.Fatal(err)
				}
			case "not directory":
				if err := os.WriteFile(target, []byte("parent is a file"), 0o600); err != nil {
					t.Fatal(err)
				}
				path += "/child"
			case "escape":
				path = "../outside.md"
			}
			err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{{Op: "delete", Path: path}}, before)
			accept := state == "absent" || state == "legacy marker"
			if accept && err != nil {
				t.Fatalf("absent delete: %v", err)
			}
			if !accept && err == nil {
				t.Fatalf("accepted %s", state)
			}
			if state == "no reload" && (err == nil || !strings.Contains(err.Error(), "did not re-parse")) {
				t.Fatalf("registry gate not exercised: %v", err)
			}
		})
	}
}

// activate promotes a snapshot the way a reload does. Load on an empty tree is
// a legitimate activation — it resolves zero projects — which is what makes it
// the right stand-in here: the point of the counter is that an activation
// counts even when nothing about the resolved content changed.
func activate(t *testing.T, r *registry.Registry, dir string) {
	t.Helper()
	if err := r.Load(dir); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

// The marker must change when the registry re-parses, even if the resolved
// project set is byte-identical — that case is exactly the one the digest
// could not report.
func TestConfigGeneration_MovesOnReparseWithIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	before := c.configGeneration()
	activate(t, c.Registry, dir)
	after := c.configGeneration()

	if before == after {
		t.Fatalf("generation marker did not move across a re-parse: %q", before)
	}
	if !strings.Contains(after, ":") {
		t.Fatalf("marker should carry a counter and a digest, got %q", after)
	}
}

// The content check that already shipped must keep working: a file that does
// not hold what the apply intended fails the journal.
func TestVerifyConfigGeneration_RefusesRevertedContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	// Something put the old bytes back after the apply.
	if err := os.WriteFile(filepath.Join(dir, "configs/swarms/x.md"), []byte("reverted"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before)
	if err == nil {
		t.Fatal("want a refusal when the target does not hold the applied content")
	}
	if !strings.Contains(err.Error(), "does not hold the applied content") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The new half: bytes correct on disk, but the registry never re-parsed. Before
// this counter existed, this passed — which is precisely CA-19.
func TestVerifyConfigGeneration_RefusesWhenTheRegistryDidNotReparse(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	// NO activation here: this is the silent-no-op reload.

	err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before)
	if err == nil {
		t.Fatal("want a refusal when the registry generation did not advance")
	}
	if !strings.Contains(err.Error(), "did not re-parse") {
		t.Fatalf("the refusal must say what was not confirmed, got: %v", err)
	}
}

// The happy path: content correct AND the registry re-parsed.
func TestVerifyConfigGeneration_AcceptsAReparseThatHoldsTheContent(t *testing.T) {
	dir := t.TempDir()
	c := &Container{ConfigPath: filepath.Join(dir, "config.yaml"), Registry: registry.New()}

	op := writeOp(t, dir, "configs/swarms/x.md", "intended")
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err != nil {
		t.Fatalf("want acceptance, got %v", err)
	}
}

// An empty op set is not a claim about anything, and must not be turned into
// one by the new check: a restart-only apply reloads nothing.
func TestVerifyConfigGeneration_EmptyOpsIsNotAClaim(t *testing.T) {
	c := &Container{ConfigPath: filepath.Join(t.TempDir(), "config.yaml"), Registry: registry.New()}
	if err := c.verifyConfigGeneration(context.Background(), nil, c.configGeneration()); err != nil {
		t.Fatalf("empty ops must pass, got %v", err)
	}
}

// §9.2c step 3. verifyConfigGeneration re-reads each op's target to confirm the
// bytes survived the reload. It resolved every path under the config dir, so a
// workspace-rooted op would send it looking in the wrong tree and fail the
// journal on a write that SUCCEEDED. Found while designing the cutover, which
// is why the cutover is four steps and not a registration.
func TestVerifyConfigGeneration_ResolvesANamedRoot(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	c := &Container{
		ConfigPath: filepath.Join(dir, "config.yaml"),
		Registry:   registry.New(),
		applyRoots: map[string]string{"workspace": ws},
	}

	op := writeOp(t, ws, "proj-1/.autonomy/PROJECT_CONTEXT.md", "the context")
	op.Path = "workspace/proj-1/.autonomy/PROJECT_CONTEXT.md"
	before := c.configGeneration()
	activate(t, c.Registry, dir)

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err != nil {
		t.Fatalf("a workspace-rooted op failed verification of a successful write: %v", err)
	}
}

// And it must still CATCH a reverted workspace write — the root awareness may
// not turn the check into a pass-through.
func TestVerifyConfigGeneration_CatchesARevertedWorkspaceWrite(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	c := &Container{
		ConfigPath: filepath.Join(dir, "config.yaml"),
		Registry:   registry.New(),
		applyRoots: map[string]string{"workspace": ws},
	}

	op := writeOp(t, ws, "proj-1/.autonomy/PROJECT_CONTEXT.md", "intended")
	op.Path = "workspace/proj-1/.autonomy/PROJECT_CONTEXT.md"
	before := c.configGeneration()
	activate(t, c.Registry, dir)
	if err := os.WriteFile(filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"), []byte("reverted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.verifyConfigGeneration(context.Background(), []controlplane.JournaledOp{op}, before); err == nil {
		t.Fatal("a reverted workspace write passed verification")
	}
}
