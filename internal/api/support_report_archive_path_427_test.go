package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/supportbundle"
)

// GitHub #427 (found at: unit, a go test ./... lane where parallel runs shared
// one TMPDIR; testing LLD §13). streamSupportBundle staged in a private
// MkdirTemp directory and then wrote the archive out of it, at
// $TMPDIR/vornik-support-<mode>.tar.gz: one name per mode, shared by every
// report of that mode and open to a name planted in advance.

func taskResult(marker string) *supportbundle.Result {
	return &supportbundle.Result{
		Files:    map[string][]byte{"task/marker.txt": []byte(marker)},
		Manifest: supportbundle.Manifest{Mode: "task"},
	}
}

func withArchiveSeam(t *testing.T, fn func(string)) {
	t.Helper()
	prev := supportArchiveWritten
	supportArchiveWritten = fn
	t.Cleanup(func() { supportArchiveWritten = prev })
}

// Two reports of one mode at once: the second starts at the moment the first
// one's archive exists. Each must stream its own whole bundle.
func TestStreamSupportBundle_ConcurrentReportsDoNotShareAnArchive_427(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var paths []string
	second := httptest.NewRecorder()
	var secondErr error
	withArchiveSeam(t, func(p string) {
		paths = append(paths, p)
		if len(paths) == 1 {
			_, _, secondErr = streamSupportBundle(second, taskResult("second"))
		}
	})

	first := httptest.NewRecorder()
	if _, _, err := streamSupportBundle(first, taskResult("first")); err != nil {
		t.Fatalf("the first report failed while a second ran: %v", err)
	}
	if secondErr != nil {
		t.Fatalf("the second report failed: %v", secondErr)
	}
	if got := string(extractBundle(t, first.Body.Bytes())["task/marker.txt"]); got != "first" {
		t.Errorf("the first report streamed %q, want its own bundle", got)
	}
	if got := string(extractBundle(t, second.Body.Bytes())["task/marker.txt"]); got != "second" {
		t.Errorf("the second report streamed %q, want its own bundle", got)
	}
	if len(paths) != 2 || filepath.Dir(paths[0]) == filepath.Dir(paths[1]) {
		t.Errorf("the two reports' archives share a directory: %v", paths)
	}
}

// A name planted in TMPDIR before the report (a symlink at the old fixed
// path) is neither followed nor used; the archive is bundle.tar.gz in the
// report's own work directory.
func TestStreamSupportBundle_PlantedNameInTMPDIRIsNotFollowed_427(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.Symlink(victim, filepath.Join(tmp, "vornik-support-task.tar.gz")); err != nil {
		t.Fatal(err)
	}
	var archive string
	withArchiveSeam(t, func(p string) { archive = p })

	rec := httptest.NewRecorder()
	if _, _, err := streamSupportBundle(rec, taskResult("mine")); err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, err := os.Lstat(victim); err == nil {
		t.Errorf("the report wrote through a symlink planted in TMPDIR (%s exists)", victim)
	}
	if got := string(extractBundle(t, rec.Body.Bytes())["task/marker.txt"]); got != "mine" {
		t.Errorf("streamed %q, want the report's own bundle", got)
	}
	dir := filepath.Clean(filepath.Dir(archive))
	if filepath.Base(archive) != "bundle.tar.gz" || dir == filepath.Clean(tmp) ||
		!strings.HasPrefix(dir, filepath.Clean(tmp)+string(filepath.Separator)) {
		t.Errorf("archive written at %s, want bundle.tar.gz in the report's own directory under %s", archive, tmp)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the report's work directory %s is left behind (stat err=%v)", dir, err)
	}
}
