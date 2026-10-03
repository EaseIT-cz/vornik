package configdrift

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Drift design slice H, review 2733 F1: the installer (scripts/config-deploy.sh)
// now reads the ack store this package writes, and that read is the only thing
// that stops it applying a template change the operator declined. A shell test
// with a hand-written ack line cannot catch the writer's format drifting away
// from the reader, so this test runs the real installer around the real
// Acknowledge, both ways: with the ack the file is kept, without it the same
// file is updated (so the first half is not passing vacuously).

func runInstaller(t *testing.T, repoConfigs, target, rev string) string {
	t.Helper()
	cmd := exec.Command("bash", "../../scripts/config-deploy.sh", target)
	cmd.Env = append(os.Environ(), "VORNIK_REPO_CONFIGS_DIR="+repoConfigs, "VORNIK_DEPLOY_REVISION="+rev)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config-deploy.sh: %v\n%s", err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerHonoursAnAckTheDaemonWrote(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, withAck := range []bool{true, false} {
		repo := filepath.Join(t.TempDir(), "configs")
		target := t.TempDir()
		configs := filepath.Join(target, "configs")
		const rel = "role-library/coder.md"
		writeFile(t, filepath.Join(repo, rel), "coder\n")
		runInstaller(t, repo, target, "rev-1")

		// The new release's template, as its baseline would show it before the
		// installer runs: the check reports a canonical divergence to ack.
		writeFile(t, filepath.Join(configs, ".templates", rel), "coder v2\n")
		if withAck {
			if _, err := Acknowledge(configs, rel, time.Now(), nil, "test:operator"); err != nil {
				t.Fatalf("Acknowledge: %v", err)
			}
		}
		writeFile(t, filepath.Join(repo, rel), "coder v2\n")
		out := runInstaller(t, repo, target, "rev-2")

		got, err := os.ReadFile(filepath.Join(configs, rel))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case withAck && string(got) != "coder\n":
			t.Errorf("an acknowledged (declined) change was applied: %q\n%s", got, out)
		case !withAck && string(got) != "coder v2\n":
			t.Errorf("without an ack the untouched file was not updated: %q\n%s", got, out)
		case !withAck && !strings.Contains(out, "updated "+rel):
			t.Errorf("the update is not in the installer's output:\n%s", out)
		}
	}
}
