package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// GitHub #581 D4 (enterprise-packaging-design.md, amendment 2026-10-29): a
// restorable backup carries secrets/ (the secret store's key, the env files)
// so the sealed secrets open on a replacement VM; the archive is then
// secret-equivalent and is written 0600; restore puts secrets/ back only
// into the directory provision made.

func writeSecretsFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"store.key": "k3y", "chat.env": "X=1", "nested/a.env": "Y=2"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStageSecrets581_CopiesFilesAt0600AndNoLinks(t *testing.T) {
	src := writeSecretsFixture(t)
	staging := t.TempDir()
	n, err := stageSecrets(src, staging)
	if err != nil || n != 3 {
		t.Fatalf("stageSecrets = %d, %v; want 3 files", n, err)
	}
	for _, name := range []string{"store.key", "chat.env", "nested/a.env"} {
		info, err := os.Stat(filepath.Join(staging, "secrets", name))
		if err != nil {
			t.Fatalf("%s not staged: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s staged at %o; want 0600", name, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(filepath.Join(staging, "secrets", "nested")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("nested dir staged at %v (%v); want 0700", info, err)
	}
	if _, err := os.Lstat(filepath.Join(staging, "secrets", "link")); err == nil {
		t.Error("a symlink was staged")
	}
}

func TestStageSecrets581_RefusesAMissingDirectory(t *testing.T) {
	if _, err := stageSecrets(filepath.Join(t.TempDir(), "absent"), t.TempDir()); err == nil {
		t.Error("a missing secrets/ was not refused; the archive would not restore")
	}
}

func TestCreatePrivateFile581_IsNeverWiderThan0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "backup.tgz")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createPrivateFile(p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Errorf("after createPrivateFile: %v %v; want an empty 0600 file", info, err)
	}
}

func TestRestoreSecrets581_PutsThemBackAt0600(t *testing.T) {
	staged := t.TempDir()
	if _, err := stageSecrets(writeSecretsFixture(t), staged); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// A key provision made on the replacement VM is overwritten by the
	// archived one: that is the one the sealed rows open with.
	if err := os.WriteFile(filepath.Join(target, "store.key"), []byte("new-vm"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := restoreSecrets(filepath.Join(staged, "secrets"), target)
	if err != nil || n != 3 {
		t.Fatalf("restoreSecrets = %d, %v; want 3", n, err)
	}
	got, err := os.ReadFile(filepath.Join(target, "store.key"))
	if err != nil || string(got) != "k3y" {
		t.Errorf("store.key = %q, %v; want the archived key", got, err)
	}
	for _, name := range []string{"store.key", "chat.env", "nested/a.env"} {
		if info, err := os.Stat(filepath.Join(target, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s restored as %v (%v); want 0600", name, info, err)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(target, ".*.restore-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporaries left: %v", leftovers)
	}
}

func TestRestoreSecrets581_RefusesWithoutProvisionsDirectory(t *testing.T) {
	staged := t.TempDir()
	if _, err := stageSecrets(writeSecretsFixture(t), staged); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreSecrets(filepath.Join(staged, "secrets"), filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a restore without provision's secrets/ was not refused")
	}
}

// Commit security review of 54502c9b2: secrets/ is the daemon user's, and
// restore runs as root, so a link planted in it must not carry a root write
// (and chown) outside it, and staging as root must not read through one.
func TestRestoreSecrets581_NeverWritesThroughALinkInTheTarget(t *testing.T) {
	staged := t.TempDir()
	if _, err := stageSecrets(writeSecretsFixture(t), staged); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreSecrets(filepath.Join(staged, "secrets"), target); err == nil {
		t.Error("restoreSecrets followed a link out of secrets/ without an error")
	}
	if _, err := os.Stat(filepath.Join(outside, "a.env")); err == nil {
		t.Fatal("a file was written outside secrets/ through a link")
	}
}

func TestRestoreSecrets581_RefusesATargetThatIsALink(t *testing.T) {
	staged := t.TempDir()
	if _, err := stageSecrets(writeSecretsFixture(t), staged); err != nil {
		t.Fatal(err)
	}
	actual := t.TempDir()
	link := filepath.Join(t.TempDir(), "secrets")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreSecrets(filepath.Join(staged, "secrets"), link); err == nil {
		t.Error("restoreSecrets accepted a secrets/ that is a link")
	}
}

func TestStageSecrets581_NeverReadsThroughALinkedDirectory(t *testing.T) {
	src := writeSecretsFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "shadow"), []byte("root-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "linkdir")); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := stageSecrets(src, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "secrets", "linkdir", "shadow")); err == nil {
		t.Error("a file was staged through a linked directory")
	}
}
