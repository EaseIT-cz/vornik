package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func TestScanStaleProposals_RetiresOnlyDrifted(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewProposalRepository(db.DB)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	freshTarget := "workflows/fresh.md"
	staleTarget := "workflows/stale.md"
	freshBytes := []byte("fresh content\n")
	if err := os.WriteFile(filepath.Join(dir, freshTarget), freshBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, staleTarget), []byte("drifted since drafting\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mkApproved := func(id, target, baseHash string) {
		ev, _ := json.Marshal(map[string]string{"base_hash": baseHash})
		p := &persistence.ControlPlaneProposal{
			ID: id, Kind: "config", BlastRadius: persistence.ProposalScopeProject,
			Title: id, Status: persistence.ProposalStatusDraft,
			ProposedBy: "tune-detector", ApplyTarget: target, ApplyContent: "x", Evidence: string(ev),
		}
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := repo.SetStatus(ctx, id, persistence.ProposalStatusApproved, "operator"); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	mkApproved("cpp_fresh", freshTarget, hashBytes(freshBytes)) // matches on-disk
	mkApproved("cpp_stale", staleTarget, "not_the_on_disk_hash")

	w := newTuneWorker(repo, nil)
	w.Actionize = &Actionizer{ReadFile: func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, rel))
	}}
	w.Logger = zerolog.Nop()

	w.scanStaleProposals(ctx)

	fresh, _ := repo.GetByID(ctx, "cpp_fresh")
	stale, _ := repo.GetByID(ctx, "cpp_stale")
	if fresh.Status != persistence.ProposalStatusApproved {
		t.Fatalf("fresh proposal must stay APPROVED, got %s", fresh.Status)
	}
	if stale.Status != persistence.ProposalStatusRejected {
		t.Fatalf("stale proposal must be retired to REJECTED, got %s", stale.Status)
	}
	if stale.Approver != AutoRetireStaleActor {
		t.Fatalf("retire approver = %q, want %q", stale.Approver, AutoRetireStaleActor)
	}
}
