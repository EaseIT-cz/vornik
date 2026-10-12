package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// GitHub #90: two output roots can contain the same logical deliverable.
// Register once only when both name and bytes agree, after pre-step gates.
func TestPersistArtifacts_DeduplicatesSameNameAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name, other string
		unchanged   bool
		want        int
	}{
		{"identical", "same", false, 1}, {"different", "different", false, 2}, {"staged", "same", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, project := t.TempDir(), t.TempDir()
			for _, d := range []string{workspace, project} {
				require.NoError(t, os.MkdirAll(filepath.Join(d, "artifacts", "out"), 0755))
			}
			p := filepath.Join(project, "artifacts", "out", "review.md")
			w := filepath.Join(workspace, "artifacts", "out", "review.md")
			require.NoError(t, os.WriteFile(p, []byte("same"), 0644))
			require.NoError(t, os.WriteFile(w, []byte(tc.other), 0644))
			store := &recordingArtifactStore{}
			e := &Executor{artifactStore: store, logger: zerolog.Nop()}
			var snap ArtifactDirSnapshot
			if tc.unchanged {
				snap = e.SnapshotArtifactDir(filepath.Dir(p))
				h, err := hashFile(w)
				require.NoError(t, err)
				snap["workspace:review.md"] = h
			}
			got, err := e.persistArtifacts(context.Background(), "exec-90", "proj", "task", workspace, project, time.Now().Add(-time.Second), snap)
			require.NoError(t, err)
			require.Len(t, got, tc.want)
		})
	}
}
