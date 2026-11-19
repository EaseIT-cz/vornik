package cli

// GitHub #801 D and #814: the github-app-review team stays diff-only. No role
// holds a tool that reads the checkout (which holds the raw lines the mask
// came from); the hunters and the adjudicator hold forge_diff_part, which
// serves only the masked diff, and the triager, which works from the lists,
// does not. Design: github-app-channel design, "Reading a large diff in parts".

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubAppReviewRoles814_DiffOnlyWithThePartReader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoConfigsDir(t), "project-templates", "github-app-review", "swarm.md.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(raw), "---", 3)
	if len(parts) < 3 {
		t.Fatal("no front matter")
	}
	front := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(parts[1], "x")
	var swarm struct {
		Roles []struct {
			Name        string
			Permissions struct {
				AllowedTools []string `yaml:"allowedTools"`
			}
		}
	}
	if err := yaml.Unmarshal([]byte(front), &swarm); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"current_time": true, "file_write": true, "forge_diff_part": true}
	wantReader := map[string]bool{"hunter_a": true, "hunter_b": true, "adjudicator": true, "triager": false}
	if len(swarm.Roles) != len(wantReader) {
		t.Fatalf("roles = %d, want %d", len(swarm.Roles), len(wantReader))
	}
	for _, r := range swarm.Roles {
		for _, tool := range r.Permissions.AllowedTools {
			if !allowed[tool] {
				t.Errorf("%s holds %s: the review roles are diff-only (#801 D)", r.Name, tool)
			}
		}
		if has := slices.Contains(r.Permissions.AllowedTools, "forge_diff_part"); has != wantReader[r.Name] {
			t.Errorf("%s: forge_diff_part = %v, want %v", r.Name, has, wantReader[r.Name])
		}
	}
}
