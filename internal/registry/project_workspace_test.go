package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #400: workspace.refresh_before_work / refresh_branch (executor design
// 06-executor.md §9.6.2, loader/validator design amendment 2026-10-17).

func loadOneProject(t *testing.T, body string) (map[string]*Project, error) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "p.yaml"), []byte(
		"projectId: p\nswarmId: s\ndefaultWorkflowId: w\n"+body), 0o644))
	return LoadProjects(dir)
}

func TestProjectWorkspace_LoadsAndDefaultsOff(t *testing.T) {
	got, err := loadOneProject(t, "")
	require.NoError(t, err)
	assert.False(t, got["p"].Workspace.RefreshBeforeWork, "off by default")

	got, err = loadOneProject(t, "workspace:\n  refresh_before_work: true\n  refresh_branch: main\n")
	require.NoError(t, err)
	assert.Equal(t, ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "main"}, got["p"].Workspace)
}

// Strict decode (loader/validator D3a): a misspelt key is refused, not ignored.
func TestProjectWorkspace_MisspeltKeyIsSkippedByTheStrictLoader(t *testing.T) {
	got, err := loadOneProject(t, "workspace:\n  refresh_on_work: true\n")
	require.NoError(t, err, "a decode error skips the file rather than failing the load")
	assert.NotContains(t, got, "p", "the misspelt file must not load as a project with the refresh off")
}

func TestProjectWorkspace_ValidatorRules(t *testing.T) {
	cases := []struct {
		name, body, field, msg string
	}{
		{"branch without the switch", "workspace:\n  refresh_branch: main\n", "workspace.refresh_branch", "would do nothing"},
		{"leading dash is an option to git fetch", "workspace:\n  refresh_before_work: true\n  refresh_branch: \"--upload-pack=x\"\n", "workspace.refresh_branch", `starts with "-"`},
		{"dot-dot", "workspace:\n  refresh_before_work: true\n  refresh_branch: a..b\n", "workspace.refresh_branch", `".."`},
		{"whitespace", "workspace:\n  refresh_before_work: true\n  refresh_branch: \"a b\"\n", "workspace.refresh_branch", "whitespace"},
		{"metacharacter", "workspace:\n  refresh_before_work: true\n  refresh_branch: \"a:b\"\n", "workspace.refresh_branch", "one of"},
		{"trailing lock", "workspace:\n  refresh_before_work: true\n  refresh_branch: main.lock\n", "workspace.refresh_branch", ".lock"},
		{"backlog mode", "workspace:\n  refresh_before_work: true\nautonomy:\n  mode: backlog\n", "workspace.refresh_before_work", "autonomy.mode: backlog"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadOneProject(t, tc.body)
			require.Error(t, err)
			var ve ProjectValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, tc.field, ve.Field)
			assert.Contains(t, ve.Message, tc.msg)
		})
	}
}

func TestPlainBranchNameProblem(t *testing.T) {
	for _, ok := range []string{"main", "release/2026.10", "feature_x-1", "v1.2"} {
		assert.Empty(t, plainBranchNameProblem(ok), ok)
	}
	for _, bad := range []string{"-x", "/a", "a/", "a//b", "a@{1}", "@", "a.", "a\tb", "a\x7fb",
		"a~1", "a^", "a?", "a*", "a[", `a\b`} {
		assert.NotEmpty(t, plainBranchNameProblem(bad), "%q must be refused", bad)
	}
}

// The same rules reach the project-form save path (ValidateProjectFile).
func TestProjectWorkspace_ValidateProjectFileAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	require.NoError(t, os.WriteFile(path, []byte("projectId: p\nswarmId: s\ndefaultWorkflowId: w\nworkspace:\n  refresh_branch: main\n"), 0o644))
	err := ValidateProjectFile(path)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "workspace.refresh_branch"), err.Error())
}
