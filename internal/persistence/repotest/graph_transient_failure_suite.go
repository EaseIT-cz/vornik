package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ChunkTransientCount reads a chunk's graph_transient_failures column.
type ChunkTransientCount func(ctx context.Context, chunkID string) (int, error)

// transientFx is one run of RunChunkGraphTransientFailureSuite.
type transientFx struct {
	t       *testing.T
	ctx     context.Context
	newRepo func() persistence.ChunkGraphExtractionRepository
	count   ChunkTransientCount
	seedOne func(id string)
}

const transientMax, transientAttempts = 10, 3

// absorb spends n uncounted retries of id, failing on any refusal.
func (fx *transientFx) absorb(repo persistence.ChunkGraphExtractionRepository, id string, n int) {
	fx.t.Helper()
	for i := 0; i < n; i++ {
		ok, err := repo.AbsorbTransientFailure(fx.ctx, id, transientMax)
		if err != nil || !ok {
			fx.t.Fatalf("absorb %d of %d for %s: absorbed=%v err=%v", i+1, n, id, ok, err)
		}
	}
}

func (fx *transientFx) wantCount(id string, want int, why string) {
	fx.t.Helper()
	got, err := fx.count(fx.ctx, id)
	if err != nil || got != want {
		fx.t.Fatalf("%s: graph_transient_failures = %d (err %v), want %d", why, got, err, want)
	}
}

func (fx *transientFx) newChunk() string {
	id := uniqueID("chunk")
	fx.seedOne(id)
	return id
}

// RunChunkGraphTransientFailureSuite pins GitHub #493 (knowledge-graph LLD
// §3.3d) on one driver: the KG worker's count of uncounted transient
// failures lives on the chunk row, so a restart (a new repository over the
// same database) does not grant a chunk a fresh budget, and the chunk
// quarantines at the same 10 + 3 failures. It drives the calls the worker
// makes, in its order.
func RunChunkGraphTransientFailureSuite(t *testing.T, newRepo func() persistence.ChunkGraphExtractionRepository, seed SeedChunk, count ChunkTransientCount) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	fxFor := func(t *testing.T) *transientFx {
		return &transientFx{t: t, ctx: ctx, newRepo: newRepo, count: count, seedOne: func(id string) {
			t.Helper()
			if err := seed(ctx, id, project, "content of "+id, true, at); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}}
	}
	t.Run("the count survives a restart and the chunk quarantines at 10 + 3", func(t *testing.T) { transientSurvivesRestart(fxFor(t)) })
	t.Run("success and a non-transient failure reset the count", func(t *testing.T) { transientResets(fxFor(t)) })
	t.Run("a counted failure under the bound keeps a spent budget", func(t *testing.T) { transientStaysSpent(fxFor(t)) })
	t.Run("an unknown chunk is not absorbed", func(t *testing.T) { transientUnknownChunk(fxFor(t)) })
}

func transientSurvivesRestart(fx *transientFx) {
	id := fx.newChunk()
	fx.absorb(fx.newRepo(), id, 6)
	restarted := fx.newRepo()
	fx.absorb(restarted, id, transientMax-6)
	if ok, err := restarted.AbsorbTransientFailure(fx.ctx, id, transientMax); err != nil || ok {
		fx.t.Fatalf("the eleventh transient failure must not be absorbed after a restart: absorbed=%v err=%v", ok, err)
	}
	for i := 1; i <= transientAttempts; i++ {
		attempts, quarantined, err := restarted.RecordExtractionFailure(fx.ctx, id, "StatusCode: 503", transientAttempts)
		if err != nil || attempts != i || quarantined != (i == transientAttempts) {
			fx.t.Fatalf("counted failure %d: attempts=%d quarantined=%v err=%v", i, attempts, quarantined, err)
		}
	}
	fx.wantCount(id, 0, "quarantine resets the count")
}

func transientResets(fx *transientFx) {
	repo := fx.newRepo()
	done, reset := fx.newChunk(), fx.newChunk()
	fx.absorb(repo, done, 4)
	if err := repo.MarkExtracted(fx.ctx, done); err != nil {
		fx.t.Fatal(err)
	}
	fx.wantCount(done, 0, "MarkExtracted resets the count")
	fx.absorb(repo, reset, transientMax)
	if err := repo.ResetTransientFailures(fx.ctx, reset); err != nil {
		fx.t.Fatal(err)
	}
	fx.wantCount(reset, 0, "ResetTransientFailures resets the count")
	fx.absorb(repo, reset, 1)
}

func transientStaysSpent(fx *transientFx) {
	repo := fx.newRepo()
	id := fx.newChunk()
	fx.absorb(repo, id, transientMax)
	if _, q, err := repo.RecordExtractionFailure(fx.ctx, id, "StatusCode: 503", transientAttempts); err != nil || q {
		fx.t.Fatalf("first counted failure: quarantined=%v err=%v", q, err)
	}
	fx.wantCount(id, transientMax, "a counted failure that does not quarantine leaves the count")
}

func transientUnknownChunk(fx *transientFx) {
	if ok, err := fx.newRepo().AbsorbTransientFailure(fx.ctx, uniqueID("missing"), transientMax); err != nil || ok {
		fx.t.Fatalf("absorbed=%v err=%v, want false, nil", ok, err)
	}
	if _, err := fx.newRepo().AbsorbTransientFailure(fx.ctx, "", transientMax); err == nil {
		fx.t.Fatal("an empty chunk id must be refused")
	}
}
