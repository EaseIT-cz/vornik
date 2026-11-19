package graph

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// errNoVector is an embedder answer with no usable vector.
var errNoVector = errors.New("graph: the embedder returned no vector")

// GitHub #459 (knowledge-graph design, amendment 2026-11-18): an entity
// inserted while the embedder failed has no vector, and SimilarByEmbedding
// never finds it. The worker re-embeds such entities in a bounded round.

const (
	reembedEvery        = 10 * time.Minute // the round's time gate: the list query scans
	reembedSkipFor      = time.Hour        // a failed entity waits this long
	reembedStopAfterBad = 3                // consecutive failures that end a round
)

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// reembedMissing runs one round when its gate allows. Its failures stay out
// of the chunk circuit breaker: a dead embedder must not stop extraction for
// reasons of its own.
func (w *Worker) reembedMissing(ctx context.Context) {
	p := w.pipeline
	if p == nil || p.Embedder == nil || p.Entities == nil {
		return
	}
	now := w.clock()
	w.mu.Lock()
	if !w.reembedLast.IsZero() && now.Sub(w.reembedLast) < reembedEvery {
		w.mu.Unlock()
		return
	}
	w.reembedLast = now
	if w.reembedSkip == nil {
		w.reembedSkip = map[string]time.Time{}
	}
	// An entry whose skip has ended is dropped, so the set holds only the
	// entities still waiting (one filled by another path leaves none).
	for id, until := range w.reembedSkip {
		if !now.Before(until) {
			delete(w.reembedSkip, id)
		}
	}
	screen := w.restrictions
	w.mu.Unlock()

	// Art 18: a restricted entity is not processed, so it is not sent to the
	// embedder either. With no screen wired the round does not run (fail
	// closed, as the graph Searcher refuses every read without one).
	if screen == nil {
		return
	}
	listed, err := p.Entities.ListMissingEmbedding(ctx, w.cfg.BatchSize)
	if err != nil {
		w.logger.Warn().Err(err).Msg("KG worker: listing entities without a vector failed")
		return
	}
	ents, err := persistence.ScreenRestrictedEntities(ctx, screen, listed)
	if err != nil {
		w.logger.Warn().Err(err).Msg("KG worker: the restriction screen failed; no entity re-embedded this round")
		return
	}
	embedded, failed, skipped, bad := 0, 0, 0, 0
	for _, e := range ents {
		if ctx.Err() != nil || bad >= reembedStopAfterBad {
			break
		}
		w.mu.Lock()
		until, skip := w.reembedSkip[e.ID]
		w.mu.Unlock()
		if skip && now.Before(until) {
			skipped++
			continue
		}
		vecs, err := p.Embedder(ctx, e.ProjectID, []string{e.CanonicalName})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			err = p.Entities.UpdateEmbedding(ctx, e.ID, vecs[0])
		} else if err == nil {
			err = errNoVector
		}
		if err != nil {
			failed++
			bad++
			w.mu.Lock()
			w.reembedSkip[e.ID] = now.Add(reembedSkipFor)
			w.mu.Unlock()
			continue
		}
		embedded++
		bad = 0
		w.mu.Lock()
		delete(w.reembedSkip, e.ID)
		w.mu.Unlock()
	}
	if w.metrics != nil && w.metrics.EntitiesReembeddedTotal != nil {
		w.metrics.EntitiesReembeddedTotal.WithLabelValues("embedded").Add(float64(embedded))
		w.metrics.EntitiesReembeddedTotal.WithLabelValues("failed").Add(float64(failed))
	}
	w.logger.Info().Int("listed", len(listed)).Int("restricted", len(listed)-len(ents)).Int("embedded", embedded).Int("failed", failed).Int("skipped", skipped).
		Msg("KG worker: re-embed round for entities without a vector")
}

// SetRestrictionScreen installs the Art 18 screen the re-embed round applies
// before it embeds anything; without one the round does not run.
func (w *Worker) SetRestrictionScreen(r persistence.RestrictionScreen) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.restrictions = r
}
