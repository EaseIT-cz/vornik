//go:build integration

package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/projectdeps"
)

// The outcome the §8 security argument rests on, with real podman and the
// real agent image (project dependency provisioning design §8.4): an offline
// install from a wheelhouse produces a tree that imports INSIDE the agent
// image, and a wheelhouse holding only a source distribution is REFUSED, so
// no package build code ever runs. The argv tests can show this only by proxy.
//
// Skipped when podman or the agent image is not present on this host.

func agentImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("VORNIK_AGENT_IMAGE")
	if img == "" {
		img = "vornik-agent:latest"
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not installed")
	}
	if err := exec.Command("podman", "image", "exists", img).Run(); err != nil {
		t.Skipf("agent image %s not present locally", img)
	}
	return img
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func recordHash(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256=" + base64.RawURLEncoding.EncodeToString(s[:])
}

// tinyWheel builds a minimal, valid pure-Python wheel; requires, when set, is
// a dependency the wheel declares.
func tinyWheel(t *testing.T, dir string, requires ...string) string {
	t.Helper()
	meta := "Metadata-Version: 2.1\nName: vorniktiny\nVersion: 1.0\n"
	for _, r := range requires {
		meta += "Requires-Dist: " + r + "\n"
	}
	files := map[string][]byte{
		"vorniktiny/__init__.py":                 []byte("MAGIC = 42\n"),
		"vorniktiny-1.0.dist-info/METADATA":      []byte(meta),
		"vorniktiny-1.0.dist-info/WHEEL":         []byte("Wheel-Version: 1.0\nGenerator: vornik-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"),
		"vorniktiny-1.0.dist-info/top_level.txt": []byte("vorniktiny\n"),
	}
	var record strings.Builder
	for name, body := range files {
		record.WriteString(name + "," + recordHash(body) + "," + strconv.Itoa(len(body)) + "\n")
	}
	record.WriteString("vorniktiny-1.0.dist-info/RECORD,,\n")
	files["vorniktiny-1.0.dist-info/RECORD"] = []byte(record.String())

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "vorniktiny-1.0-py3-none-any.whl")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return sha256Hex(buf.Bytes())
}

// tinySdist builds a source distribution only, with a setup.py that would
// leave a marker file if a build backend ever ran it.
func tinySdist(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"vornikbuild-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: vornikbuild\nVersion: 1.0\n",
		"vornikbuild-1.0/setup.py": "open('/staging/BUILD-CODE-RAN', 'w').write('x')\nfrom setuptools import setup\nsetup(name='vornikbuild', version='1.0')\n",
	} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0)})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(filepath.Join(dir, "vornikbuild-1.0.tar.gz"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return sha256Hex(buf.Bytes())
}

func integrationInput(t *testing.T, img, lock string) depsInstallInput {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "ws", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "requirements.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	return depsInstallInput{
		ProjectID: "proj", ProjectRoot: project, CacheDir: filepath.Join(root, "deps"),
		Entries:    []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}},
		RoleImages: map[string][]string{img: {"coder"}},
	}
}

func TestIntegrationDepsInstall_WheelImportsInsideTheImage(t *testing.T) {
	img := agentImage(t)
	wheels := t.TempDir()
	sum := tinyWheel(t, wheels)
	in := integrationInput(t, img, "vorniktiny==1.0 \\\n    --hash=sha256:"+sum+"\n")
	in.Wheelhouse = wheels
	var out bytes.Buffer
	if err := runDepsInstall(context.Background(), in, execRunner, &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	store := projectdeps.NewStore(in.CacheDir)
	plan := projectdeps.NewResolver(store, "").Plan(in.ProjectRoot, in.Entries)[0]
	meta, err := store.ReadMarker(plan.Key)
	if err != nil || !meta.ListsImage(img) || meta.Interpreter == "" {
		t.Fatalf("marker %+v %v", meta, err)
	}
	// Import it inside the image through the same mount and env the daemon
	// gives an agent.
	mount := plan.Mount(store)
	env := projectdeps.InjectEnv([]projectdeps.Mount{mount}, "/usr/bin:/bin")
	args := []string{"run", "--rm", "--pull=never", "--network=none", "-v", mount.VolumeSpec()}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, "--entrypoint", "python3", img, "-c", "import vorniktiny; print(vorniktiny.MAGIC)")
	got, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil || strings.TrimSpace(string(got)) != "42" {
		t.Fatalf("the installed tree must import inside the agent image: %v\n%s", err, got)
	}
}

func TestIntegrationDepsInstall_SdistIsRefusedAndNeverBuilt(t *testing.T) {
	img := agentImage(t)
	wheels := t.TempDir()
	sum := tinySdist(t, wheels)
	in := integrationInput(t, img, "vornikbuild==1.0 \\\n    --hash=sha256:"+sum+"\n")
	in.Wheelhouse = wheels
	var out bytes.Buffer
	err := runDepsInstall(context.Background(), in, execRunner, &out)
	// Refused BY --only-binary, not by something incidental (a permission or
	// mount failure would also "refuse"; the first version of this test passed
	// on exactly that, 2026-09-25).
	if err == nil || !strings.Contains(err.Error(), "No matching distribution") {
		t.Fatalf("an sdist-only requirement must be refused by pip's binary-only rule, got %v\n%s", err, out.String())
	}
	entries, _ := os.ReadDir(in.CacheDir)
	for _, e := range entries {
		t.Errorf("a refused install must leave nothing in the cache, found %s", e.Name())
	}
}

// With --require-hashes and without --no-deps, a lockfile that does not pin a
// dependency its packages declare is REFUSED rather than installed half
// complete (review-20260925-0325 F8).
func TestIntegrationDepsInstall_IncompleteLockfileIsRefused(t *testing.T) {
	img := agentImage(t)
	wheels := t.TempDir()
	sum := tinyWheel(t, wheels, "vornikmissing")
	in := integrationInput(t, img, "vorniktiny==1.0 \\\n    --hash=sha256:"+sum+"\n")
	in.Wheelhouse = wheels
	var out bytes.Buffer
	err := runDepsInstall(context.Background(), in, execRunner, &out)
	if err == nil || !strings.Contains(err.Error(), "vornikmissing") {
		t.Fatalf("a lockfile missing a declared dependency must be refused naming it, got %v\n%s", err, out.String())
	}
	entries, _ := os.ReadDir(in.CacheDir)
	for _, e := range entries {
		t.Errorf("a refused install must leave nothing in the cache, found %s", e.Name())
	}
}
