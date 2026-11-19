package repotest

import (
	"context"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// manifestStates802 holds what each case of the #802 suite needs.
type manifestStates802 struct {
	t    *testing.T
	repo persistence.GitHubAppManifestStateRepository
	ctx  context.Context
	now  time.Time
	u    string // a per-run prefix: a shared Postgres lane keeps earlier runs' rows
}

func (m manifestStates802) put(state, session string, at time.Time) {
	m.t.Helper()
	if err := m.repo.PutManifestState(m.ctx, m.u+state, m.u+session, "https://ghes.example/api/v3", at, at.Add(time.Hour)); err != nil {
		m.t.Fatalf("put %s: %v", state, err)
	}
}

func (m manifestStates802) class(state, session string, at time.Time) persistence.ManifestStateClass {
	m.t.Helper()
	c, err := m.repo.ClassifyManifestState(m.ctx, m.u+state, m.u+session, at)
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

func (m manifestStates802) consume(state, session string, at time.Time) (string, bool) {
	m.t.Helper()
	base, ok, err := m.repo.ConsumeManifestState(m.ctx, m.u+state, m.u+session, at)
	if err != nil {
		m.t.Fatal(err)
	}
	return base, ok
}

// RunGitHubAppManifestStateSuite is GitHub #802's state contract
// (github-app-channel design, "One-click App creation", Data): a state is
// consumed once, by the session that made it, inside its hour; a session
// holds at most one live state.
func RunGitHubAppManifestStateSuite(t *testing.T, repo persistence.GitHubAppManifestStateRepository) {
	t.Helper()
	m := func(t *testing.T) manifestStates802 {
		return manifestStates802{t: t, repo: repo, ctx: context.Background(),
			now: time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC), u: uniqueID("gh") + "-"}
	}
	t.Run("consumed_once_by_its_session_802", func(t *testing.T) { manifestConsumedOnce802(m(t)) })
	t.Run("expires_after_its_hour_802", func(t *testing.T) { manifestExpires802(m(t)) })
	t.Run("a_second_put_supersedes_the_session's_first_802", func(t *testing.T) { manifestSupersedes802(m(t)) })
	t.Run("two_concurrent_consumes_win_once_802", func(t *testing.T) { manifestConcurrent802(m(t)) })
}

func manifestConsumedOnce802(m manifestStates802) {
	t, now := m.t, m.now
	m.put("st-a", "sess-1", now)
	if c := m.class("st-a", "sess-2", now); c != persistence.ManifestStateOtherSession {
		t.Errorf("another session's state classed %q", c)
	}
	if _, ok := m.consume("st-a", "sess-2", now); ok {
		t.Fatal("another session consumed the state")
	}
	if c := m.class("st-a", "sess-1", now); c != persistence.ManifestStateLive {
		t.Errorf("a live state classed %q", c)
	}
	if base, ok := m.consume("st-a", "sess-1", now.Add(time.Minute)); !ok || base != "https://ghes.example/api/v3" {
		t.Fatalf("its own session could not consume it: %q %v", base, ok)
	}
	if _, ok := m.consume("st-a", "sess-1", now.Add(2*time.Minute)); ok {
		t.Fatal("a state was consumed twice")
	}
	if c := m.class("st-a", "sess-1", now); c != persistence.ManifestStateUsed {
		t.Errorf("a used state classed %q", c)
	}
	if c := m.class("st-none", "sess-1", now); c != persistence.ManifestStateMissing {
		t.Errorf("an unknown state classed %q", c)
	}
}

func manifestExpires802(m manifestStates802) {
	exp := m.now.Add(time.Hour)
	m.put("st-b", "sess-3", m.now)
	if _, ok := m.consume("st-b", "sess-3", exp.Add(time.Second)); ok {
		m.t.Fatal("an expired state was consumed")
	}
	if c := m.class("st-b", "sess-3", exp.Add(time.Second)); c != persistence.ManifestStateExpired {
		m.t.Errorf("an expired state classed %q", c)
	}
}

func manifestSupersedes802(m manifestStates802) {
	m.put("st-c1", "sess-4", m.now)
	m.put("st-c2", "sess-4", m.now.Add(time.Minute))
	if _, ok := m.consume("st-c1", "sess-4", m.now.Add(2*time.Minute)); ok {
		m.t.Fatal("the superseded state was consumed")
	}
	if _, ok := m.consume("st-c2", "sess-4", m.now.Add(2*time.Minute)); !ok {
		m.t.Fatal("the newer state could not be consumed")
	}
}

func manifestConcurrent802(m manifestStates802) {
	m.put("st-d", "sess-5", m.now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := m.repo.ConsumeManifestState(m.ctx, m.u+"st-d", m.u+"sess-5", m.now.Add(time.Minute))
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		m.t.Fatalf("%d concurrent consumes won, want exactly 1", wins)
	}
}
