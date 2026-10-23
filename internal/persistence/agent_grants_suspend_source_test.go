package persistence

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. The held read tools on agent_integration_approvals have exactly
// two writers per driver (agent-administered design §6.5, amendment #481,
// change item 2): SuspendIntegrationTools adds, and UpsertIntegration's
// re-add branch resets. A third writer would be a way to clear a held tool
// without the person's re-approval, so this pins the denominator: every
// non-test Go file under internal/ that names the column, and every
// assignment to it.
// suspendedAssign matches an assignment or comparison of the column in any
// spelling (adversarial review F4: a literal match missed "x='[]'").
var suspendedAssign = regexp.MustCompile(`suspended_tools\s*=`)

func TestAgentGrantSuspendedTools481_OnlyTwoWritersPerDriver(t *testing.T) {
	allowedFiles := map[string]bool{
		"persistence/migrations.go":                      true,
		"persistence/sqlite/schema.go":                   true,
		"persistence/sqlite/sqlite.go":                   true,
		"persistence/sqlite/agent_grant_repository.go":   true,
		"persistence/postgres/agent_grant_repository.go": true,
	}
	root := ".."
	var named []string
	assignments := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(root, path, d.Name(), "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "suspended_tools") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		named = append(named, rel)
		assignments[rel] = len(suspendedAssign.FindAllIndex(b, -1))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(named)
	for _, f := range named {
		if !allowedFiles[f] {
			t.Errorf("%s names suspended_tools: only the two grant repositories may write it (design §6.5, amendment #481)", f)
		}
	}
	if len(named) != len(allowedFiles) {
		t.Errorf("files naming suspended_tools: %v, want exactly %d", named, len(allowedFiles))
	}
	// Per driver: the re-add CASE in UpsertIntegration and the
	// compare-and-set UPDATE in SuspendIntegrationTools (whose WHERE also
	// reads the column, hence three textual "suspended_tools = ").
	for _, drv := range []string{"persistence/sqlite/agent_grant_repository.go", "persistence/postgres/agent_grant_repository.go"} {
		if assignments[drv] != 3 {
			t.Errorf("%s has %d \"suspended_tools = \" occurrences, want 3 (re-add CASE, CAS SET, CAS WHERE)", drv, assignments[drv])
		}
	}
}
