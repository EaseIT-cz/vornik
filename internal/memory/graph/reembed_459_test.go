package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #459 (knowledge-graph design, amendment 2026-11-18). Found at:
// review (the #369 outcome-quality audit), observed as entities invisible to
// vector matching. An entity inserted while the embedder failed kept a NULL
// vector forever. Regression test at: unit (the worker's tick); the
// repository half is the repotest suite on both drivers.

// reembedRepo is an entity store that keeps vectors.
type reembedRepo struct {
	fakeEntityRepo
	ents map[string]*persistence.KnowledgeEntity
}

func (r *reembedRepo) ListMissingEmbedding(_ context.Context, limit int) ([]*persistence.KnowledgeEntity, error) {
	var out []*persistence.KnowledgeEntity
	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		if e, ok := r.ents[id]; ok && e.Embedding == nil && e.LifecycleState != "superseded" {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *reembedRepo) UpdateEmbedding(_ context.Context, id string, vec []float32) error {
	if e := r.ents[id]; e != nil && e.Embedding == nil {
		e.Embedding = vec
	}
	return nil
}

type screen459 struct{ restricted map[string]bool }

func (s screen459) RestrictedEntities(_ context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if s.restricted[id] {
			out[id] = true
		}
	}
	return out, nil
}
func (screen459) RestrictedChunks(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// emptySource has no chunks, so a tick is the re-embed round alone.
type emptySource struct {
	persistence.ChunkGraphExtractionRepository
}

func (emptySource) FetchUnextracted(context.Context, int) ([]persistence.ChunkForExtraction, error) {
	return nil, nil
}

func worker459(repo *reembedRepo, embed EmbedFn, restricted map[string]bool) (*Worker, *time.Time) {
	now := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	w := NewWorker(emptySource{}, &Pipeline{Entities: repo, Embedder: embed}, zerolog.Nop(), WorkerConfig{BatchSize: 10})
	w.now = func() time.Time { return now }
	w.SetRestrictionScreen(screen459{restricted: restricted})
	return w, &now
}

func ent459(id string) *persistence.KnowledgeEntity {
	return &persistence.KnowledgeEntity{ID: id, ProjectID: "p-" + id, Type: "person", CanonicalName: "Name " + id, LifecycleState: "published"}
}

func TestReembed_AnOutageLeftAVectorlessEntityAndALaterRoundFillsIt_459(t *testing.T) {
	repo := &reembedRepo{ents: map[string]*persistence.KnowledgeEntity{"e1": ent459("e1")}}
	down := true
	var scopes []string
	w, now := worker459(repo, func(_ context.Context, projectID string, _ []string) ([][]float32, error) {
		scopes = append(scopes, projectID)
		if down {
			return nil, errors.New("embedder down")
		}
		return [][]float32{{0.1, 0.2}}, nil
	}, nil)
	w.tick(context.Background())
	if repo.ents["e1"].Embedding != nil {
		t.Fatal("an embedder that is down filled a vector")
	}
	down = false
	*now = now.Add(reembedSkipFor + time.Minute) // past the gate and the entity's skip
	w.tick(context.Background())
	if repo.ents["e1"].Embedding == nil {
		t.Fatal("a later round with the embedder back left the entity without a vector")
	}
	if len(scopes) != 2 || scopes[1] != "p-e1" {
		t.Fatalf("embed scopes %v, want the entity's own project", scopes)
	}
}

func TestReembed_FailuresSkipStopAndGate_459(t *testing.T) {
	repo := &reembedRepo{ents: map[string]*persistence.KnowledgeEntity{
		"e1": ent459("e1"), "e2": ent459("e2"), "e3": ent459("e3"), "e4": ent459("e4"),
	}}
	calls := 0
	bad := map[string]bool{"Name e1": true}
	w, now := worker459(repo, func(_ context.Context, _ string, texts []string) ([][]float32, error) {
		calls++
		if bad[texts[0]] {
			return nil, errors.New("refused name")
		}
		return [][]float32{{1}}, nil
	}, map[string]bool{"e4": true})
	w.tick(context.Background())
	// e1 fails and is skipped; e2 and e3 are filled behind it; e4 is restricted.
	if repo.ents["e1"].Embedding != nil || repo.ents["e2"].Embedding == nil || repo.ents["e3"].Embedding == nil {
		t.Fatalf("one failing entity starved the rest: %+v", repo.ents)
	}
	if repo.ents["e4"].Embedding != nil || calls != 3 {
		t.Fatalf("a restricted entity was sent to the embedder (calls %d)", calls)
	}
	// A second tick inside the gate does nothing.
	w.tick(context.Background())
	if calls != 3 {
		t.Fatalf("a tick inside the 10-minute gate embedded again (calls %d)", calls)
	}
	// Past the gate but inside the skip, e1 is not retried.
	*now = now.Add(reembedEvery + time.Second)
	w.tick(context.Background())
	if calls != 3 {
		t.Fatalf("a skipped entity was retried inside its skip (calls %d)", calls)
	}
	// Three consecutive failures end a round.
	repo2 := &reembedRepo{ents: map[string]*persistence.KnowledgeEntity{"e1": ent459("e1"), "e2": ent459("e2"), "e3": ent459("e3"), "e4": ent459("e4")}}
	calls2 := 0
	w2, _ := worker459(repo2, func(context.Context, string, []string) ([][]float32, error) {
		calls2++
		return nil, errors.New("down")
	}, nil)
	w2.tick(context.Background())
	if calls2 != reembedStopAfterBad {
		t.Fatalf("a dead embedder was asked %d times in one round, want %d", calls2, reembedStopAfterBad)
	}
	// Without a restriction screen the round does not run.
	w3 := NewWorker(emptySource{}, &Pipeline{Entities: repo2, Embedder: func(context.Context, string, []string) ([][]float32, error) {
		t.Fatal("embedded with no restriction screen")
		return nil, nil
	}}, zerolog.Nop(), WorkerConfig{BatchSize: 10})
	w3.tick(context.Background())
}

type failingScreen459 struct{}

func (failingScreen459) RestrictedEntities(context.Context, []string) (map[string]bool, error) {
	return nil, errors.New("screen down")
}
func (failingScreen459) RestrictedChunks(context.Context, []string) (map[string]bool, error) {
	return nil, errors.New("screen down")
}

// A screen that fails ends the round before any embed: fail closed.
func TestReembed_AFailingScreenEmbedsNothing_459(t *testing.T) {
	repo := &reembedRepo{ents: map[string]*persistence.KnowledgeEntity{"e1": ent459("e1")}}
	w := NewWorker(emptySource{}, &Pipeline{Entities: repo, Embedder: func(context.Context, string, []string) ([][]float32, error) {
		t.Fatal("embedded although the restriction screen failed")
		return nil, nil
	}}, zerolog.Nop(), WorkerConfig{BatchSize: 10})
	w.SetRestrictionScreen(failingScreen459{})
	w.tick(context.Background())
	if repo.ents["e1"].Embedding != nil {
		t.Fatal("an entity was filled past a failing screen")
	}
}
