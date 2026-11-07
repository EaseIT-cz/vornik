package contractreg

import (
	"os"
	"path/filepath"
	"testing"
)

// The CE export lives in .vornik-export/ and its tests run INSIDE it. Pruning
// by directory name alone skipped the walk root itself, so the audit reported
// zero integration-tagged files in a tree that held 36 — and the export refused
// to publish on a blind parser rather than a real fault.
func TestAuditBuildTags_ScansARootNamedLikeAnExportCopy(t *testing.T) {
	root := t.TempDir()
	// A directory whose name carries the export prefix, standing in for
	// .vornik-export as the walk ROOT rather than a child.
	exportish := filepath.Join(root, ".vornik-export")
	if err := os.MkdirAll(filepath.Join(exportish, "internal", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "//go:build integration\n\npackage pkg\n"
	if err := os.WriteFile(filepath.Join(exportish, "internal", "pkg", "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	audit, err := AuditBuildTags(exportish, nil)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if got := len(audit.FilesByTag["integration"]); got != 1 {
		t.Errorf("found %d integration-tagged files in an export-named ROOT, want 1", got)
	}
}

// A nested export copy must still be skipped, or every tag double-counts.
func TestAuditBuildTags_StillSkipsANestedExportCopy(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"internal/pkg", ".vornik-export/internal/pkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		src := "//go:build integration\n\npackage pkg\n"
		if err := os.WriteFile(filepath.Join(root, dir, "x_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	audit, err := AuditBuildTags(root, nil)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if got := len(audit.FilesByTag["integration"]); got != 1 {
		t.Errorf("found %d files, want 1 — the nested export copy was counted twice", got)
	}
}

// Toolchain-set constraints (GOOS, GOARCH, unix, cgo, goN.M) are set by
// every build of a matching platform, not by build plumbing. Found at:
// lint, 2026-11-05: //go:build unix files (GitHub #461) were reported as
// never compiled, and linux passed only because one Makefile smoke target
// happens to pass -tags=linux.
func TestAuditBuildTags_ToolchainConstraintsAreNotPlumbingTags(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.go": "//go:build unix\n\npackage pkg\n",
		"b.go": "//go:build !unix && !windows\n\npackage pkg\n",
		"c.go": "//go:build linux && amd64 && cgo\n\npackage pkg\n",
		"d.go": "//go:build go1.22 && smoke_codex\n\npackage pkg\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := AuditBuildTags(root, nil)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	got := audit.NeverSet()
	if len(got) != 1 || got[0] != "smoke_codex" {
		t.Errorf("NeverSet = %v, want only [smoke_codex]", got)
	}
}
