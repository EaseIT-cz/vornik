package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/artifacts"
)

// GitHub #359 — audit 2026-10-12 (W3-11 independent review, F1): the
// step-output harvest checked each artifacts/out entry by path
// (safepath.JoinUnder) and then re-opened the path by name to hash and store
// it. On the warm path the agent's container is still running during the
// harvest, so a background process can swap the checked file (or its
// directory) for a symlink to another project's stored input between the
// check and the read, and the daemon stores those bytes as an output row of
// the harvesting project. harvestBeforeOpenHook is the deterministic seam
// between the listing and the read; the race is not reproduced
// stochastically.

const otherProjectSecret = "PROJECT-B-STORED-INPUT-359"

// harvestRaceFixture lays out a store holding project B's input, and a
// project-A workspace whose artifacts/out/r.pdf is the agent's own file.
func harvestRaceFixture(t *testing.T) (workspace, foreignFile string, store *artifacts.Store, stored storedOutputs) {
	t.Helper()
	storeBase := t.TempDir()
	foreignDir := filepath.Join(storeBase, "projB", "inputs", "artifact_b")
	require.NoError(t, os.MkdirAll(foreignDir, 0o755))
	foreignFile = filepath.Join(foreignDir, "r.pdf")
	require.NoError(t, os.WriteFile(foreignFile, []byte(otherProjectSecret), 0o644))

	workspace = t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "r.pdf"), []byte("agent's own report"), 0o644))

	s, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)
	return workspace, foreignFile, s, storedOutputs(filepath.Join(storeBase, "projA"))
}

// storedOutputs is project A's subtree of the store: everything the harvest
// wrote for A.
type storedOutputs string

// bodies returns the content of every file stored under project A.
func (d storedOutputs) bodies(t *testing.T) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(string(d), func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		require.NoError(t, rerr)
		out = append(out, string(b))
		return nil
	})
	return out
}

func setHarvestHook(t *testing.T, fn func(name string)) {
	t.Helper()
	prev := harvestBeforeOpenHook
	harvestBeforeOpenHook = fn
	t.Cleanup(func() { harvestBeforeOpenHook = prev })
}

func requireNoForeignBytesStored(t *testing.T, stored storedOutputs) {
	t.Helper()
	for _, body := range stored.bodies(t) {
		require.NotContains(t, body, otherProjectSecret,
			"#359: the harvest stored another project's bytes as a project-A output")
	}
}

func TestPersistArtifacts_SymlinkSwapAfterListingIsNotFollowed(t *testing.T) {
	t.Run("file swapped for a symlink", func(t *testing.T) {
		workspace, foreign, store, repo := harvestRaceFixture(t)
		target := filepath.Join(workspace, "artifacts", "out", "r.pdf")
		setHarvestHook(t, func(name string) {
			if name != "r.pdf" {
				return
			}
			_ = os.Remove(target)
			require.NoError(t, os.Symlink(foreign, target))
		})
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, _ = e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", workspace, "", time.Now().Add(-time.Minute), nil)
		requireNoForeignBytesStored(t, repo)
	})

	t.Run("directory swapped for a symlink", func(t *testing.T) {
		workspace, foreign, store, repo := harvestRaceFixture(t)
		outDir := filepath.Join(workspace, "artifacts", "out")
		setHarvestHook(t, func(name string) {
			if name != "r.pdf" {
				return
			}
			require.NoError(t, os.RemoveAll(outDir))
			require.NoError(t, os.Symlink(filepath.Dir(foreign), outDir))
		})
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, _ = e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", workspace, "", time.Now().Add(-time.Minute), nil)
		requireNoForeignBytesStored(t, repo)
	})

	t.Run("project-persisted tree entry swapped for a symlink", func(t *testing.T) {
		_, foreign, store, repo := harvestRaceFixture(t)
		projectDir := t.TempDir()
		outDir := filepath.Join(projectDir, "artifacts", "out")
		require.NoError(t, os.MkdirAll(outDir, 0o755))
		target := filepath.Join(outDir, "r.pdf")
		require.NoError(t, os.WriteFile(target, []byte("agent's own deliverable"), 0o644))
		setHarvestHook(t, func(name string) {
			if name != "r.pdf" {
				return
			}
			_ = os.Remove(target)
			require.NoError(t, os.Symlink(foreign, target))
		})
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, _ = e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", t.TempDir(), projectDir, time.Time{}, nil)
		requireNoForeignBytesStored(t, repo)
	})

	// Review round 1 (F2): the root itself is held as one descriptor from
	// the listing to the last open, so even a swap of the harvest root's
	// host path (which a container cannot do: it is the mount point) does
	// not change which directory is read.
	t.Run("harvest root swapped after listing", func(t *testing.T) {
		workspace, _, store, repo := harvestRaceFixture(t)
		// Another tree holding the other project's bytes as plain files.
		foreignWS := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(foreignWS, "artifacts", "out"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(foreignWS, "artifacts", "out", "r.pdf"), []byte(otherProjectSecret), 0o644))
		swapped := false
		setHarvestHook(t, func(string) {
			if swapped {
				return
			}
			swapped = true
			require.NoError(t, os.Rename(workspace, workspace+".moved"))
			require.NoError(t, os.Symlink(foreignWS, workspace))
		})
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, _ = e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", workspace, "", time.Now().Add(-time.Minute), nil)
		require.True(t, swapped)
		requireNoForeignBytesStored(t, repo)
		_ = os.Remove(workspace)
		require.NoError(t, os.Rename(workspace+".moved", workspace))
	})

	// Review round 1 (F3): absolute symlinks are refused even when they
	// name a file inside the workspace, which covers /proc/self/fd/N magic
	// links to a descriptor the daemon holds.
	t.Run("absolute symlinks are refused", func(t *testing.T) {
		workspace, foreign, store, repo := harvestRaceFixture(t)
		f, err := os.Open(foreign)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		outDir := filepath.Join(workspace, "artifacts", "out")
		require.NoError(t, os.Symlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), filepath.Join(outDir, "magic.pdf")))
		require.NoError(t, os.Symlink(filepath.Join(outDir, "r.pdf"), filepath.Join(outDir, "abs-in-root.pdf")))
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, err = e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", workspace, "", time.Now().Add(-time.Minute), nil)
		require.NoError(t, err)
		requireNoForeignBytesStored(t, repo)
		// Only r.pdf itself is stored; neither absolute link is.
		require.Len(t, repo.bodies(t), 1)
	})

	t.Run("an in-root relative symlink is still harvested", func(t *testing.T) {
		workspace, _, store, repo := harvestRaceFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.md"), []byte("in-root notes"), 0o644))
		require.NoError(t, os.Symlink("../../notes.md", filepath.Join(workspace, "artifacts", "out", "link.md")))
		e := &Executor{artifactStore: store, logger: zerolog.Nop()}
		_, err := e.persistArtifacts(context.Background(), "exec-a", "projA", "task-a", workspace, "", time.Now().Add(-time.Minute), nil)
		require.NoError(t, err)
		n := 0
		for _, body := range repo.bodies(t) {
			if body == "in-root notes" {
				n++
			}
		}
		// notes.md itself (workspace root) and link.md (artifacts/out).
		require.Equal(t, 2, n, "a relative symlink that stays beneath the workspace must still be harvested")
	})
}

// A FIFO in artifacts/out must be refused, not opened in blocking mode: a
// blocking open waits for a writer forever and hangs the harvest (#359, same
// reads).
func TestPersistArtifacts_FIFOIsRefusedNotBlocking(t *testing.T) {
	workspace := t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	fifo := filepath.Join(outDir, "pipe.md")
	require.NoError(t, syscall.Mkfifo(fifo, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "ok.md"), []byte("ok"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}
	done := make(chan error, 1)
	go func() {
		_, err := e.persistArtifacts(context.Background(), "exec-f", "projA", "task-f", workspace, "", time.Now().Add(-time.Minute), nil)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		// Unblock the stuck open so the test binary can finish.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
		<-done
		t.Fatal("#359: the harvest blocked on a FIFO in artifacts/out")
	}
	require.Len(t, store.seenNames, 1, "only the regular file is harvested; got %v", store.seenNames)
}

// A harvest root that cannot be opened yields no outputs; that must be
// visible, not look like a step that produced none (#359 review round 2). A
// missing directory stays quiet.
func TestPersistArtifacts_UnopenableRootIsLogged(t *testing.T) {
	var buf bytes.Buffer
	e := &Executor{artifactStore: &recordingArtifactStore{}, logger: zerolog.New(&buf)}
	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o644))

	_, err := e.persistArtifacts(context.Background(), "exec", "projA", "task", notADir, filepath.Join(t.TempDir(), "missing"), time.Time{}, nil)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "harvest root could not be opened")
	require.Equal(t, 1, strings.Count(buf.String(), "harvest root could not be opened"),
		"the missing project directory must not be logged")
}
