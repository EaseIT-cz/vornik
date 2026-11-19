package registry

// GitHub #812 (github-app-channel design, Amendment 2026-11-18 (#812)): a
// hand-made github, forge.github or github_app block may not use the
// instance's App key. The refusal compares file contents, so a copy, a
// symlink, a hard link and any path spelling are refused alike. Found at:
// review; observed at: integration (the registry loading a project file).
// Test at: integration, through the loader's Stage.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyFiles812(t *testing.T) (instance, other string) {
	t.Helper()
	dir := t.TempDir()
	instance = filepath.Join(dir, "github-app.pem")
	other = filepath.Join(dir, "own.pem")
	if err := os.WriteFile(instance, []byte("-----BEGIN RSA PRIVATE KEY-----\ninstance\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("-----BEGIN RSA PRIVATE KEY-----\nown\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return instance, other
}

func tree812(block string) map[string]string {
	files := baseAgentTree()
	files["projects/plain.yaml"] = agentProjectYAML("plain", "hermes--fin", "hermes--fin--start", block)
	return files
}

func githubBlock812(kind, keyPath string) string {
	switch kind {
	case "github_app":
		return "github_app:\n  app_id: 1\n  installation_id: 99\n  private_key_path: " + keyPath + "\n  webhook_secret_env: X\n  repo_allowlist: [\"o/r\"]\n"
	case "forge.github":
		return "forge:\n  provider: github\n  github:\n    app_id: 1\n    installation_id: 99\n    private_key_path: " + keyPath + "\n    repo: o/r\n"
	}
	return "github:\n  app_id: 1\n  installation_id: 99\n  private_key_path: " + keyPath + "\n"
}

const refusal812 = "instance's GitHub App key"

func TestInstanceAppKey812_HandMadeBlocksMayNotUseTheInstanceKey(t *testing.T) {
	instance, other := keyFiles812(t)
	SetInstanceAppKeyPath(instance)
	t.Cleanup(func() { SetInstanceAppKeyPath("") })
	dir := filepath.Dir(instance)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(instance, link); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard.pem")
	if err := os.Link(instance, hard); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(dir, "copy.pem")
	raw, _ := os.ReadFile(instance)
	_ = os.WriteFile(cp, raw, 0o600)
	dotted := filepath.Join(dir, ".", "github-app.pem")

	for _, kind := range []string{"github", "forge.github", "github_app"} {
		for name, path := range map[string]string{"path": instance, "symlink": link, "hard link": hard, "copy": cp, "another spelling": dotted} {
			if rs := rejectedFor(t, tree812(githubBlock812(kind, path))); !hasRejection(rs, refusal812) {
				t.Errorf("%s naming the instance key by %s loaded: %+v", kind, name, rs)
			}
		}
		for name, path := range map[string]string{"its own key": other, "a missing file": filepath.Join(dir, "missing.pem")} {
			if rs := rejectedFor(t, tree812(githubBlock812(kind, path))); hasRejection(rs, refusal812) {
				t.Errorf("%s naming %s was refused as the instance key: %+v", kind, name, rs)
			}
		}
	}
	if rs := rejectedFor(t, tree812("forge:\n  provider: github\n  github:\n    use_registration: true\n    repo: o/r\n")); hasRejection(rs, refusal812) {
		t.Errorf("a use_registration block was refused: %+v", rs)
	}
}

// No App registered: nothing to compare. The App registered later is seen
// at the next load.
func TestInstanceAppKey812_AKeyThatAppearsLaterIsSeenAtTheNextLoad(t *testing.T) {
	instance, _ := keyFiles812(t)
	raw, _ := os.ReadFile(instance)
	cp := filepath.Join(filepath.Dir(instance), "copy.pem")
	_ = os.WriteFile(cp, raw, 0o600)
	_ = os.Remove(instance)
	SetInstanceAppKeyPath(instance)
	t.Cleanup(func() { SetInstanceAppKeyPath("") })
	files := tree812(githubBlock812("github", cp))
	if rs := rejectedFor(t, files); hasRejection(rs, refusal812) {
		t.Fatalf("refused with no instance key: %+v", rs)
	}
	_ = os.WriteFile(instance, raw, 0o600) // the App is registered
	if rs := rejectedFor(t, files); !hasRejection(rs, refusal812) {
		t.Fatalf("the next load did not see the newly registered key: %+v", rs)
	}
	if !strings.Contains(refusal812, "GitHub App key") {
		t.Fatal("unreachable")
	}
}
