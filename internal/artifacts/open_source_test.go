package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GitHub #359 — audit 2026-10-12 (W3-11 independent review, F1): Store and
// StoreInput re-opened their source path following symlinks and blocking on a
// FIFO. The executor's harvest no longer hands them agent-changeable paths
// (it uses StoreFile), but the path entry points refuse a final-component
// symlink and a non-regular file as defence in depth.
func TestOpenRegularNoFollow_RefusesSymlinkAndFIFO(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("OTHER-PROJECT-BYTES"), 0o644))
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(secret, link))
	fifo := filepath.Join(dir, "pipe.txt")
	require.NoError(t, syscall.Mkfifo(fifo, 0o644))

	s, err := New(WithBasePath(t.TempDir()))
	require.NoError(t, err)

	_, err = s.Store(context.Background(), "projA", "exec-1", "task-1", "link.txt", link)
	require.Error(t, err, "Store must not follow a final-component symlink")
	_, err = s.StoreInput(context.Background(), "projA", "link.txt", link)
	require.Error(t, err, "StoreInput must not follow a final-component symlink")

	done := make(chan error, 1)
	go func() {
		_, err := s.Store(context.Background(), "projA", "exec-1", "task-1", "pipe.txt", fifo)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "Store must refuse a FIFO")
	case <-time.After(10 * time.Second):
		if w, werr := os.OpenFile(fifo, os.O_WRONLY, 0); werr == nil {
			_ = w.Close()
		}
		<-done
		t.Fatal("#359: Store blocked opening a FIFO")
	}

	// A regular file still stores.
	a, err := s.Store(context.Background(), "projA", "exec-1", "task-1", "secret.txt", secret)
	require.NoError(t, err)
	body, err := os.ReadFile(a.StoragePath)
	require.NoError(t, err)
	require.Equal(t, "OTHER-PROJECT-BYTES", string(body))
}
