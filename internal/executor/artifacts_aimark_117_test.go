package executor

// GitHub #117, Art 50(2) step 2 commit 5 (Art 50 design §14.8, second
// amendment): every file an agent produces gets a ledger row when it is
// harvested, so it is found by its hash whichever tool made it. A ledger
// failure never fails the harvest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/persistence"
)

type harvestMarkLedger struct {
	mu     sync.Mutex
	hashes []string
	fail   bool
}

func (l *harvestMarkLedger) Record(_ context.Context, m persistence.AIOutputMark) error {
	if l.fail {
		return errors.New("db down")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hashes = append(l.hashes, m.ContentSHA256)
	return nil
}
func (l *harvestMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func TestPersistArtifacts117_LedgerRowPerHarvestedFile(t *testing.T) {
	for _, fail := range []bool{false, true} {
		workspace, project := t.TempDir(), t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(workspace, "artifacts", "out"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "artifacts", "out", "report.pdf"), []byte("%PDF-agent"), 0o644))
		ledger := &harvestMarkLedger{fail: fail}
		e := &Executor{artifactStore: &recordingArtifactStore{}, logger: zerolog.Nop()}
		e.aiMarker = aimark.NewMarker(aimark.NewSigner(make([]byte, 32)), ledger, func() string { return "t" }, nil, nil)
		got, err := e.persistArtifacts(context.Background(), "exec-1", "proj", "task", workspace, project, time.Now().Add(-time.Second), nil)
		require.NoError(t, err, "a ledger failure must never fail the harvest")
		require.Len(t, got, 1)
		if fail {
			continue
		}
		sum := sha256.Sum256([]byte("%PDF-agent"))
		require.Equal(t, []string{hex.EncodeToString(sum[:])}, ledger.hashes)
	}
}
