//go:build e2e_http
// +build e2e_http

// Package e2e_test — process-spawn law S3 on the real binary
// (https://docs.vornik.io).
//
// Until 2026-09-26 the daemon ran git on request: control-plane apply and
// rollback committed the mirrored source tree, and the UI artifact delete
// committed the deletion in the project workspace. S3 removed the commits.
// These tests prove the CAPABILITIES survive: an applied control-plane proposal
// still lands (and is mirrored into the operator's checkout), a rollback still
// restores, an artifact delete still deletes — and the git repositories the
// daemon touches gain no commit.
//
// Workflow proposals are the third S3 surface; they are Postgres-only (the
// SQLite repository is a stub that stores nothing), so their end-to-end lane is
// internal/service/workflow_proposal_apply_integration_test.go.
package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A control-plane proposal (the wizard's scaffold) applies and rolls back
// through the real apply engine and mirror, with the source checkout a git
// repository whose HEAD must not move.
func TestS3_ControlPlaneProposalApplyAndRollbackMakeNoCommit(t *testing.T) {
	const projectID = "e2e-s3-proj"
	headBefore := gitIn(t, sourceRoot, "rev-parse", "HEAD")
	proposal := `{"raw":{"projectId":"` + projectID + `","displayName":"E2E S3 Project","topic":"spawns"}}`
	sid := seedSession(t, &persistence.ProjectWizardSession{
		ID:                sessionID("s3"),
		OperatorID:        e2eOperatorID,
		Transcript:        []byte("[]"),
		CurrentProposal:   []byte(proposal),
		SuggestedTemplate: e2eTemplateSlug,
		ReadyToCommit:     true,
	})
	resp, body := doReq(t, http.MethodPost, "/api/v1/projects/wizard/"+sid+"/commit", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit: HTTP %d, body=%s", resp.StatusCode, body)
	}
	var cr struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal([]byte(body), &cr); err != nil || cr.ProposalID == "" {
		t.Fatalf("commit body: %v (%s)", err, body)
	}
	if resp, b := doReq(t, http.MethodPost, "/api/v1/operator/proposals/"+cr.ProposalID+"/decide",
		"application/json", strings.NewReader(`{"decision":"approve","actor":"e2e-reviewer"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: HTTP %d, body=%s", resp.StatusCode, b)
	}
	if resp, b := doReq(t, http.MethodPost, "/api/v1/operator/proposals/"+cr.ProposalID+"/apply",
		"application/json", strings.NewReader(`{"actor":"e2e-reviewer"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: HTTP %d, body=%s", resp.StatusCode, b)
	}

	deployed := filepath.Join(filepath.Dir(dbPath), "configs", "projects", projectID+".yaml")
	mirrored := filepath.Join(sourceRoot, "configs", "projects", projectID+".yaml")
	if _, err := os.Stat(deployed); err != nil {
		t.Fatalf("apply did not write the deployed project file: %v", err)
	}
	if _, err := os.Stat(mirrored); err != nil {
		t.Fatalf("apply was not mirrored into the source checkout: %v", err)
	}
	if head := gitIn(t, sourceRoot, "rev-parse", "HEAD"); head != headBefore {
		t.Fatalf("apply made a git commit in the source checkout: %s → %s", headBefore, head)
	}

	if resp, b := doReq(t, http.MethodPost, "/api/v1/operator/proposals/"+cr.ProposalID+"/rollback",
		"application/json", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("rollback: HTTP %d, body=%s", resp.StatusCode, b)
	}
	if _, err := os.Stat(deployed); !os.IsNotExist(err) {
		t.Errorf("rollback did not remove the file the apply created: err=%v", err)
	}
	if _, err := os.Stat(mirrored); !os.IsNotExist(err) {
		t.Errorf("rollback was not mirrored into the source checkout: err=%v", err)
	}
	if head := gitIn(t, sourceRoot, "rev-parse", "HEAD"); head != headBefore {
		t.Errorf("rollback made a git commit in the source checkout: %s → %s", headBefore, head)
	}
}

// The UI artifact delete removes the file and makes no commit in the project's
// workspace repository.
func TestS3_UIArtifactDeleteMakesNoCommit(t *testing.T) {
	ws := filepath.Join(workspaceRoot, e2eStaticProject)
	if err := os.MkdirAll(filepath.Join(ws, "artifacts", "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "artifacts", "out", "note.md")
	if err := os.WriteFile(target, []byte("an artifact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ws, "init", "-q")
	gitIn(t, ws, "add", ".")
	gitIn(t, ws, "-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid", "commit", "-q", "-m", "seed")
	headBefore := gitIn(t, ws, "rev-parse", "HEAD")

	form := url.Values{"path": {"out/note.md"}}
	resp, body := doReq(t, http.MethodPost, "/ui/projects/"+e2eStaticProject+"/artifacts/delete",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("artifact delete: HTTP %d, body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "ok=") {
		t.Fatalf("artifact delete redirected to %q, want the success banner", loc)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the artifact still exists: err=%v", err)
	}
	if head := gitIn(t, ws, "rev-parse", "HEAD"); head != headBefore {
		t.Fatalf("the delete made a git commit in the project workspace: %s → %s", headBefore, head)
	}
}
