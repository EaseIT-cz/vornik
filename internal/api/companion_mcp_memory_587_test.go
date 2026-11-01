package api

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/firstvalue"
)

// milestoneStore is a first-wins in-memory InstanceMilestoneRepository.
type milestoneStore struct{ rows map[string]time.Time }

func (m *milestoneStore) MarkMilestone(_ context.Context, name string, at time.Time) (bool, error) {
	if _, ok := m.rows[name]; ok {
		return false, nil
	}
	m.rows[name] = at
	return true, nil
}

func (m *milestoneStore) Milestones(context.Context) (map[string]time.Time, error) {
	return m.rows, nil
}

// GitHub #587 (operations-api-contract-design.md §7.2.2): a companion recall
// that returns a hit is the instance's first recall; one with no hits is not.
func TestCompanionRecall_MarksTheFirstRecallOnAHit(t *testing.T) {
	for _, tc := range []struct {
		name string
		hits []MemorySearchResult
		want bool
	}{
		{"a hit", []MemorySearchResult{{ChunkID: "ck1", ProjectID: "alpha", SourceName: "s", Content: "hit", Score: 0.9}}, true},
		{"no hits", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &milestoneStore{rows: map[string]time.Time{}}
			tr := firstvalue.New(store, zerolog.Nop())
			srv, keyRepo, _ := newCompanionMCPServer(t)
			srv.firstValue = tr
			srv.memoryCompanion = &fakeMemoryCompanion{recallReturn: tc.hits}
			_, key := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)
			_, err := srv.companionToolRecall(context.Background(), key, []byte(`{"query":"x"}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, got := store.rows[firstvalue.FirstRecall]; got != tc.want {
				t.Fatalf("first_recall marked = %v; want %v", got, tc.want)
			}
		})
	}
}
