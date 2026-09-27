package projectdeps

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsMaterialisedRequiresTheMarkerNotJustTheDirectory(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if err := os.MkdirAll(filepath.Join(s.Path("k1"), "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Mounting a partial tree presents the missing half as an import
	// error inside an agent, which is the failure this design removes.
	done, err := s.IsMaterialised("k1")
	if err != nil {
		t.Fatalf("IsMaterialised() = %v", err)
	}
	if done {
		t.Fatal("a directory without the completion marker must not read as materialised")
	}
}

func TestStoreRootAndPath(t *testing.T) {
	root := "/var/lib/vornik/deps"
	s := NewStore(root)
	if s.Root() != root {
		t.Fatalf("Root() = %q, want %q", s.Root(), root)
	}
	if got, want := s.Path("k1"), filepath.Join(root, "k1"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestIsMaterialisedSurfacesAStatErrorRatherThanReportingAbsent(t *testing.T) {
	// A key path that is a FILE makes the marker stat fail with ENOTDIR,
	// which is neither "present" nor "absent". Reporting absent would
	// send an installer to refetch into a path it can never publish.
	root := t.TempDir()
	s := NewStore(root)
	if err := os.WriteFile(s.Path("k1"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.IsMaterialised("k1"); err == nil {
		t.Fatal("IsMaterialised() = nil error, want the stat failure surfaced")
	}
}

// The marker is JSON the daemon reads to decide whether a tree serves a role
// image (design §8.2); a marker that does not parse is an error, not "absent".
func TestReadMarker(t *testing.T) {
	s := NewStore(t.TempDir())
	meta := MarkerMeta{Key: "k1", Images: []string{"vornik-agent:latest"}, ImageIDs: map[string]string{"vornik-agent:latest": "sha256:1"}, Interpreter: "cpython-312 x86_64 glibc-2.39"}
	body, err := EncodeMarker(meta)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, s.Path("k1"), map[string]string{CompletionMarker: string(body)})
	got, err := s.ReadMarker("k1")
	if err != nil || !got.ListsImage("vornik-agent:latest") || got.ListsImage("other:1") || got.Interpreter != meta.Interpreter {
		t.Fatalf("ReadMarker = %+v, %v", got, err)
	}
	writeTree(t, s.Path("k2"), map[string]string{CompletionMarker: "key: k2\n"})
	if _, err := s.ReadMarker("k2"); err == nil {
		t.Fatal("a marker that does not parse must be an error")
	}
}
