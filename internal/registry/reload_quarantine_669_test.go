package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// GitHub #669 item 2 and #683 (loader-validator agreement design §15): a
// refused PROJECT file no longer blocks the rest of a reload. The running
// project is carried over on its last good definition (quarantined) and the
// other changes apply; a refused swarm or workflow still refuses; and §12's
// rule is re-checked, so a running project is never dropped.

const otherProject = `projectId: "other"
displayName: "Other"
swarmId: "s1"
defaultWorkflowId: "w1"
`

func twoProjectRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	dir := projectTree(t, map[string]string{"keeper.yaml": goodProject, "other.yaml": otherProject})
	r := New()
	if err := r.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	return r, dir
}

func write669(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReload669_QuarantinesARefusedProjectAndAppliesTheRest(t *testing.T) {
	r, dir := twoProjectRegistry(t)
	write669(t, dir, "projects/keeper.yaml", aheadOfBinaryProject)
	write669(t, dir, "projects/other.yaml", `projectId: "other"
displayName: "Other, renamed"
swarmId: "s1"
defaultWorkflowId: "w1"
`)
	err := r.Reload()
	var q *ReloadQuarantinedError
	if !errors.As(err, &q) {
		t.Fatalf("reload = %v, want a ReloadQuarantinedError (the rest applies)", err)
	}
	if len(q.Projects) != 1 || q.Projects[0] != "keeper" {
		t.Fatalf("quarantined %v, want [keeper]", q.Projects)
	}
	if p := r.GetProject("other"); p == nil || p.DisplayName != "Other, renamed" {
		t.Fatalf("the other project's change must apply: %+v", p)
	}
	if p := r.GetProject("keeper"); p == nil || p.DisplayName != "Keeper" {
		t.Fatalf("keeper must keep serving its last good definition: %+v", p)
	}
}

func TestReload669_ARefusedSwarmStillRefusesTheReload(t *testing.T) {
	r, dir := twoProjectRegistry(t)
	write669(t, dir, "projects/other.yaml", `projectId: "other"
displayName: "Other, renamed"
swarmId: "s1"
defaultWorkflowId: "w1"
`)
	write669(t, dir, "swarms/s1.md", "---\nswarmId: \"s1\"\ndisplayName: [unclosed\nroles:\n  - name: \"coder\"\n---\n")
	// A swarm that does not parse fails the stage (or is refused by the gate):
	// either way the reload fails and nothing applies.
	err := r.Reload()
	var q *ReloadQuarantinedError
	if err == nil || errors.As(err, &q) {
		t.Fatalf("reload = %v, want a refusal for a refused swarm", err)
	}
	if p := r.GetProject("other"); p == nil || p.DisplayName != "Other" {
		t.Fatalf("nothing may apply when a swarm is refused: %+v", p)
	}
}

func TestReload669_ARenamedRefusedFileStillRefuses(t *testing.T) {
	r, dir := twoProjectRegistry(t)
	if err := os.Remove(filepath.Join(dir, "projects", "keeper.yaml")); err != nil {
		t.Fatal(err)
	}
	write669(t, dir, "projects/keeper-renamed.yaml", aheadOfBinaryProject)
	err := r.Reload()
	var rej *ReloadRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("reload = %v, want ReloadRejectedError: a running project would be dropped", err)
	}
	if r.GetProject("keeper") == nil {
		t.Fatal("keeper must survive")
	}
}

func TestReload669_ANewProjectsRefusedFileAddsNothingAndAppliesTheRest(t *testing.T) {
	r, dir := twoProjectRegistry(t)
	write669(t, dir, "projects/newcomer.yaml", `projectId: "newcomer"
displayName: "New"
swarmId: "s1"
defaultWorkflowId: "w1"
mystery: true
`)
	write669(t, dir, "projects/other.yaml", `projectId: "other"
displayName: "Other, renamed"
swarmId: "s1"
defaultWorkflowId: "w1"
`)
	err := r.Reload()
	var q *ReloadQuarantinedError
	if !errors.As(err, &q) || len(q.Projects) != 0 || len(q.Rejected) != 1 {
		t.Fatalf("reload = %v, want a ReloadQuarantinedError naming the refused file and no project", err)
	}
	if r.GetProject("newcomer") != nil {
		t.Fatal("a refused new project must not appear")
	}
	if p := r.GetProject("other"); p == nil || p.DisplayName != "Other, renamed" {
		t.Fatalf("the rest must apply: %+v", p)
	}
}

// A carried project the new tree no longer satisfies (its swarm is gone, so
// the strip removes it) refuses the reload: it is never dropped.
func TestReload669_ACarriedProjectThatNoLongerValidatesRefuses(t *testing.T) {
	r, dir := twoProjectRegistry(t)
	write669(t, dir, "projects/keeper.yaml", aheadOfBinaryProject)
	write669(t, dir, "projects/other.yaml", `projectId: "other"
displayName: "Other"
swarmId: "s2"
defaultWorkflowId: "w1"
`)
	write669(t, dir, "swarms/s2.md", "---\nswarmId: \"s2\"\ndisplayName: \"S2\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n")
	if err := os.Remove(filepath.Join(dir, "swarms", "s1.md")); err != nil {
		t.Fatal(err)
	}
	err := r.Reload()
	var rej *ReloadRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("reload = %v, want ReloadRejectedError: carried keeper references a removed swarm", err)
	}
	if r.GetProject("keeper") == nil {
		t.Fatal("keeper must survive the refused reload")
	}
}
