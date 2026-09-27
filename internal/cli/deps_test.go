package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
)

// `vornikctl deps install` (project dependency provisioning design §8,
// 2026-09-25): the operator installs a project's declared dependencies INSIDE
// the agent image; the daemon only mounts. Driven through a runner seam so no
// test needs podman.

const depsLock = "pkg==1.0 \\\n    --hash=sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"

type depsFakeRunner struct {
	ids     map[string]string // image -> id; absent = not present locally
	triples map[string]string // image -> interpreter triple
	calls   [][]string
	pipErr  error
}

func (f *depsFakeRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if args[0] == "image" {
		img := args[len(args)-1]
		if id, ok := f.ids[img]; ok {
			return []byte(id + "\n"), nil
		}
		return nil, errors.New("image not known")
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, " -c ") {
		for img, tr := range f.triples {
			if strings.Contains(joined, " "+img+" ") {
				return []byte(tr), nil
			}
		}
		return nil, errors.New("no python3")
	}
	if strings.Contains(joined, "pip install") {
		if f.pipErr != nil {
			return []byte("ERROR: no matching distribution (only binary)"), f.pipErr
		}
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/staging:rw,z") {
				dir := strings.TrimSuffix(args[i+1], ":/staging:rw,z")
				if err := os.MkdirAll(filepath.Join(dir, projectdeps.SiteDir, "pkg"), 0o755); err != nil {
					return nil, err
				}
				return nil, os.WriteFile(filepath.Join(dir, projectdeps.SiteDir, "pkg", "__init__.py"), []byte(""), 0o644)
			}
		}
		return nil, errors.New("no staging mount")
	}
	return nil, nil
}

func depsFixture(t *testing.T) depsInstallInput {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "ws", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "requirements.lock"), []byte(depsLock), 0o644); err != nil {
		t.Fatal(err)
	}
	return depsInstallInput{
		ProjectID:   "proj",
		ProjectRoot: project,
		CacheDir:    filepath.Join(root, "deps"),
		Entries:     []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}},
		RoleImages:  map[string][]string{"vornik-agent:latest": {"coder", "reviewer"}},
	}
}

func goodRunner() *depsFakeRunner {
	return &depsFakeRunner{
		ids:     map[string]string{"vornik-agent:latest": "sha256:aaa"},
		triples: map[string]string{"vornik-agent:latest": "cpython-312 x86_64 glibc-2.39"},
	}
}

func TestDepsInstall_InstallsInsideTheImageAndRecordsIt(t *testing.T) {
	in := depsFixture(t)
	f := goodRunner()
	var out bytes.Buffer
	if err := runDepsInstall(context.Background(), in, f.run, &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	for _, want := range []string{"vornik-agent:latest", "sha256:aaa", "coder", "reviewer", "installed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	plans := projectdeps.NewResolver(projectdeps.NewStore(in.CacheDir), "").Plan(in.ProjectRoot, in.Entries)
	meta, err := projectdeps.NewStore(in.CacheDir).ReadMarker(plans[0].Key)
	if err != nil || !meta.ListsImage("vornik-agent:latest") || meta.ImageIDs["vornik-agent:latest"] != "sha256:aaa" || meta.Interpreter == "" {
		t.Fatalf("marker %+v %v", meta, err)
	}
	// Idempotent: a second run runs no pip.
	f.calls = nil
	out.Reset()
	if err := runDepsInstall(context.Background(), in, f.run, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "pip install") {
			t.Fatal("an installed key must not run pip again")
		}
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("want 'already installed':\n%s", out.String())
	}
}

func TestDepsInstall_RefusesAnImageNotPresentLocally(t *testing.T) {
	in := depsFixture(t)
	f := goodRunner()
	delete(f.ids, "vornik-agent:latest")
	err := runDepsInstall(context.Background(), in, f.run, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "never pulled") {
		t.Fatalf("want a refusal, got %v", err)
	}
	for _, c := range f.calls {
		if c[0] == "run" {
			t.Fatal("nothing may run in an image before every image is confirmed local")
		}
	}
}

func TestDepsInstall_RefusesRoleImagesOnDifferentPythons(t *testing.T) {
	in := depsFixture(t)
	in.RoleImages["other:1"] = []string{"writer"}
	f := goodRunner()
	f.ids["other:1"] = "sha256:bbb"
	f.triples["other:1"] = "cpython-311 x86_64 glibc-2.39"
	err := runDepsInstall(context.Background(), in, f.run, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "other:1") || !strings.Contains(err.Error(), "vornik-agent:latest") {
		t.Fatalf("want a refusal naming both images, got %v", err)
	}
}

func TestDepsInstall_FromWheelhouseAndTheHintOnFailure(t *testing.T) {
	in := depsFixture(t)
	in.Wheelhouse = "/mnt/wheels"
	f := goodRunner()
	if err := runDepsInstall(context.Background(), in, f.run, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "pip install") {
			found = strings.Contains(j, "--network=none") && strings.Contains(j, "--no-index") && strings.Contains(j, "/mnt/wheels:/wheels:ro,z")
		}
	}
	if !found {
		t.Fatalf("--from must install offline from the wheelhouse: %v", f.calls)
	}

	in2 := depsFixture(t)
	f2 := goodRunner()
	f2.pipErr = errors.New("exit 1")
	var out bytes.Buffer
	err := runDepsInstall(context.Background(), in2, f2.run, &out)
	if err == nil || !strings.Contains(out.String(), "pip download") {
		t.Fatalf("a failed connected install must print the wheelhouse command: err=%v\n%s", err, out.String())
	}
}

func TestDepsInstall_TreeForOtherImagesIsNotReplacedSilently(t *testing.T) {
	in := depsFixture(t)
	if err := runDepsInstall(context.Background(), in, goodRunner().run, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	in.RoleImages = map[string][]string{"vornik-agent:v2": {"coder"}}
	f := goodRunner()
	f.ids["vornik-agent:v2"] = "sha256:ccc"
	f.triples["vornik-agent:v2"] = "cpython-312 x86_64 glibc-2.39"
	err := runDepsInstall(context.Background(), in, f.run, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "vornik-agent:latest") || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("a tree installed for other images must be named, not overwritten: %v", err)
	}
}

func TestDepsInstall_NothingDeclared(t *testing.T) {
	in := depsFixture(t)
	in.Entries = nil
	var out bytes.Buffer
	if err := runDepsInstall(context.Background(), in, goodRunner().run, &out); err != nil || !strings.Contains(out.String(), "declares no dependencies") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestDepsStatus_ReportsImageDrift(t *testing.T) {
	in := depsFixture(t)
	if err := runDepsInstall(context.Background(), in, goodRunner().run, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f := goodRunner()
	f.ids["vornik-agent:latest"] = "sha256:rebuilt"
	var out bytes.Buffer
	if err := runDepsStatus(context.Background(), []depsInstallInput{in}, f.run, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proj", "installed", "sha256:aaa", "changed since install"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}
