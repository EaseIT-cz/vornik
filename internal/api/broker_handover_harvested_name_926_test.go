package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #926 (production, 2026-11-25 19:04 and 19:12): both hand-overs of
// venue-enrichment-with-rag were refused missing_document. The harvest
// stores an output as <stem>-<YYYYMMDD>-<exec suffix><ext>, and
// handOverDocument compared that to the bare declared name, so it could
// never match. The #920 tests seeded "scout.md", a shape the harvest never
// writes.
//
// Found at: production (two refused hand-overs in the daemon log).
// Regression test at: unit (FireHandover over a mocked artifact repository
// holding the harvested row shape).
// Lower because: a live run needs Hermes's workflow and its connections;
// the row shape the harvest writes is reproduced exactly.
func TestFireHandover_FindsAHarvestedDisambiguatedName_926(t *testing.T) {
	rig := newHandOverRig(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "scout-20261125-a8f2.md")
	require.NoError(t, os.WriteFile(p, []byte("# harvested venues\n"), 0o600))
	ar := &mocks.MockArtifactRepository{}
	ar.ListFunc = func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{
			// A different file whose stem shares the prefix must not match.
			{ID: "a0", ProjectID: "hermes--research", Name: "scout-notes-20261125-a8f2.md",
				ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p, CreatedAt: time.Now()},
			{ID: "a1", ProjectID: "hermes--research", Name: "scout-20261125-a8f2.md",
				ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p, CreatedAt: time.Now()},
		}, nil
	}
	rig.srv.artifactRepo = ar
	_, err := rig.fire()
	require.NoError(t, err, "a harvested output must satisfy the hand-over")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rig.tasks.LastCall.Task.Payload, &payload))
	require.Equal(t, "# harvested venues\n", payload["context"].(map[string]any)["broker_inputs"].(map[string]any)["venues"])
}
