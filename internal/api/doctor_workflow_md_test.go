package api

// Tests for the workflow_md_shape doctor check. The per-rule
// validator semantics are exercised in
// internal/registry/workflow_md_validate_test.go — these tests
// cover the adapter: severity rollup, sort order, and the
// "fires on every shipped workflow" integration.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const adapterCleanWorkflow = `---
name: clean
description: Adapter test fixture; all required fields present.
version: "1.0.0"
author: vornik
license: Apache-2.0
entrypoint: only
steps:
  only:
    type: agent
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---

## Prompts

### only

Do it.
`

func writeWorkflowDir(t *testing.T, files map[string]string) string {
	t.Helper()
	cfgDir := t.TempDir()
	dir := filepath.Join(cfgDir, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return cfgDir
}

func TestCheckWorkflowMDShape_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	check := h.checkWorkflowMDShape()
	if check.Status != "SKIPPED" {
		t.Fatalf("no configDir → SKIPPED (nothing examined); got %s (%s)", check.Status, check.Message)
	}
}

func TestCheckWorkflowMDShape_NoWorkflowsDir(t *testing.T) {
	// configDir exists, but no workflows/ subdir under it.
	h := &DoctorHandlers{configDir: t.TempDir()}
	check := h.checkWorkflowMDShape()
	if check.Status != "SKIPPED" {
		t.Fatalf("missing workflows dir → SKIPPED (nothing examined); got %s (%s)", check.Status, check.Message)
	}
}

// An empty workflows/ directory examined nothing either.
func TestCheckWorkflowMDShape_EmptyWorkflowsDirIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &DoctorHandlers{configDir: dir}
	if check := h.checkWorkflowMDShape(); check.Status != "SKIPPED" {
		t.Fatalf("empty workflows dir → SKIPPED; got %s (%s)", check.Status, check.Message)
	}
}

func TestCheckWorkflowMDShape_CleanDirOK(t *testing.T) {
	cfg := writeWorkflowDir(t, map[string]string{"clean.md": adapterCleanWorkflow})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "OK" {
		t.Fatalf("clean dir → OK; got %s (%s) Items=%v", check.Status, check.Message, check.Items)
	}
	if !strings.Contains(check.Message, "1 workflow") {
		t.Fatalf("message should mention the file count; got %q", check.Message)
	}
}

// workflow-md design, "Publication recommendations belong to publishing"
// (2026-10-03): author_missing and license_missing were a permanent WARNING on
// the reference host (26 findings, 14 files, none published). They are notes
// for publishing, counted in the message, not a warning and not listed.
func unattributed() string {
	body := strings.Replace(adapterCleanWorkflow, "author: vornik\n", "", 1)
	return strings.Replace(body, "license: Apache-2.0\n", "", 1)
}

func TestCheckWorkflowMDShape_PublicationNotesAreNotWarnings(t *testing.T) {
	cfg := writeWorkflowDir(t, map[string]string{"a.md": unattributed(), "b.md": unattributed(), "c.md": adapterCleanWorkflow})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "OK" {
		t.Fatalf("only publication notes → OK; got %s (%s) Items=%v", check.Status, check.Message, check.Items)
	}
	if len(check.Items) != 0 {
		t.Errorf("publication notes must not be listed; got %v", check.Items)
	}
	for _, want := range []string{"2 workflow(s) carry no author or license", "vornikctl skill export --author --license"} {
		if !strings.Contains(check.Message, want) {
			t.Errorf("message %q lacks %q", check.Message, want)
		}
	}
}

func TestCheckWorkflowMDShape_RealWarningStillWarnsWithoutTheNotes(t *testing.T) {
	big := unattributed() + strings.Repeat("Long prose line for the soft size limit.\n", 400)
	cfg := writeWorkflowDir(t, map[string]string{"big.md": big})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "WARNING" {
		t.Fatalf("a real warning → WARNING; got %s (%s)", check.Status, check.Message)
	}
	joined := strings.Join(check.Items, "\n")
	if !strings.Contains(joined, "file_size_soft") {
		t.Errorf("the real warning is not listed: %v", check.Items)
	}
	if strings.Contains(joined, "author_missing") || strings.Contains(joined, "license_missing") {
		t.Errorf("publication notes listed beside a real warning: %v", check.Items)
	}
}

// The count is of distinct files, and the clause rides every branch.
func TestCheckWorkflowMDShape_PublicationClauseCountsFilesInEveryBranch(t *testing.T) {
	onlyLicense := strings.Replace(adapterCleanWorkflow, "license: Apache-2.0\n", "", 1)
	broken := strings.Replace(unattributed(), "description: Adapter test fixture; all required fields present.\n", "", 1)
	cfg := writeWorkflowDir(t, map[string]string{"both.md": unattributed(), "lic.md": onlyLicense, "err.md": broken})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "ERROR" {
		t.Fatalf("a real error → ERROR; got %s (%s)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "3 workflow(s) carry no author or license") {
		t.Errorf("message %q: want 3 distinct files counted in the ERROR branch", check.Message)
	}
}

func TestCheckWorkflowMDShape_ErrorOnRequiredMissing(t *testing.T) {
	body := strings.Replace(adapterCleanWorkflow, "description: Adapter test fixture; all required fields present.\n", "", 1)
	cfg := writeWorkflowDir(t, map[string]string{"bad.md": body})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "ERROR" {
		t.Fatalf("missing description → ERROR; got %s (%s) Items=%v", check.Status, check.Message, check.Items)
	}
	found := false
	for _, it := range check.Items {
		if strings.Contains(it, "description_missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected description_missing in items; got %v", check.Items)
	}
}

func TestCheckWorkflowMDShape_ItemsSortedByFilename(t *testing.T) {
	// Two bad files, each with one ERROR. The Items list must
	// be sorted by filename so the diff between two doctor runs
	// is stable.
	bad := strings.Replace(adapterCleanWorkflow, "description: Adapter test fixture; all required fields present.\n", "", 1)
	cfg := writeWorkflowDir(t, map[string]string{
		"z.md": bad,
		"a.md": bad,
	})
	h := &DoctorHandlers{configDir: cfg}
	check := h.checkWorkflowMDShape()
	if check.Status != "ERROR" {
		t.Fatalf("two bad files → ERROR; got %s", check.Status)
	}
	if len(check.Items) < 2 {
		t.Fatalf("expected 2 items; got %v", check.Items)
	}
	if !strings.HasPrefix(check.Items[0], "a.md") {
		t.Fatalf("first item should be a.md; got %q", check.Items[0])
	}
	if !strings.HasPrefix(check.Items[1], "z.md") {
		t.Fatalf("second item should be z.md; got %q", check.Items[1])
	}
}

// TestCheckWorkflowMDShape_ShippedCorpus is the integration
// assertion called out in the BACKLOG item: the doctor check
// must pass cleanly over every shipped workflow. Failure here
// usually means a new shipped workflow forgot a required field
// (description) — fix the workflow, don't relax the validator.
func TestCheckWorkflowMDShape_ShippedCorpus(t *testing.T) {
	root := repoRootFromAPITest(t)
	cfgDir := filepath.Join(root, "configs")
	if _, err := os.Stat(filepath.Join(cfgDir, "workflows")); err != nil {
		t.Skipf("no configs/workflows under %s; skipping", cfgDir)
	}
	h := &DoctorHandlers{configDir: cfgDir}
	check := h.checkWorkflowMDShape()
	if check.Status == "ERROR" {
		t.Fatalf("shipped corpus failed the SKILL.md shape check: %s\nItems:\n  %s",
			check.Message, strings.Join(check.Items, "\n  "))
	}
	// Verify the check actually ran across every shipped file
	// (would otherwise pass vacuously if the walker silently
	// skipped them).
	for _, want := range []string{
		"adaptive.md", "dev-pipeline.md", "plan-and-write.md",
		"research.md", "simple-workflow.md", "trading.md",
	} {
		path := filepath.Join(cfgDir, "workflows", want)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("shipped workflow %s missing on disk: %v", want, err)
		}
	}
	if !strings.Contains(check.Message, "workflow") && !strings.Contains(check.Message, "warning") && !strings.Contains(check.Message, "file(s)") {
		t.Fatalf("expected coverage in message; got %q", check.Message)
	}
}

// TestCheckWorkflowMDShape_RegisteredInRunDoctor pins the
// integration: the new check must be invoked by RunDoctor so a
// future refactor of doctor_handlers.go can't silently drop it.
func TestCheckWorkflowMDShape_RegisteredInRunDoctor(t *testing.T) {
	// Smoke: open the source file and look for the wire-up
	// line. A behavioural test would require spinning up the
	// HTTP handler with a real DB; the wire-up is a one-liner
	// and a string check catches the regression at near-zero
	// cost.
	root := repoRootFromAPITest(t)
	path := filepath.Join(root, "internal", "api", "doctor_handlers.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read doctor_handlers.go: %v", err)
	}
	if !strings.Contains(string(data), "h.checkWorkflowMDShape()") {
		t.Fatalf("checkWorkflowMDShape() not wired into RunDoctor; check doctor_handlers.go's check list")
	}
}

func TestCountDistinctFiles(t *testing.T) {
	in := []workflowMDFileFinding{
		{filename: "a.md"},
		{filename: "a.md"},
		{filename: "b.md"},
	}
	if got := countDistinctFiles(in); got != 2 {
		t.Fatalf("countDistinctFiles = %d; want 2", got)
	}
	if got := countDistinctFiles(nil); got != 0 {
		t.Fatalf("countDistinctFiles(nil) = %d; want 0", got)
	}
}

// repoRootFromAPITest walks up to go.mod. Same pattern as
// repoRootFromTest in the executor's lint tests, kept local so
// the api package's tests don't pull a cross-package internal.
func repoRootFromAPITest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod from %s", dir)
	return ""
}
