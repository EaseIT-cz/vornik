package api

// GitHub #617 slice 2 (federation design, "Slice 2: the corpus-walk
// finding"): a caller key that has walked an expert's corpus is named by
// doctor, with how many distinct chunks its A2A tasks retrieved in the last
// 24 hours, never which chunks.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func corpusWalkCheck(t *testing.T, rows []persistence.A2AKeyRetrieval, err error) (DoctorCheck, time.Time) {
	t.Helper()
	h := &DoctorHandlers{}
	var since time.Time
	h.SetA2AKeyRetrieval(func(_ context.Context, s time.Time) ([]persistence.A2AKeyRetrieval, error) {
		since = s
		return rows, err
	})
	return h.checkA2ACorpusWalk(context.Background()), since
}

func TestA2ACorpusWalk_UnwiredIsSkipped_617(t *testing.T) {
	c := (&DoctorHandlers{}).checkA2ACorpusWalk(context.Background())
	if c.Name != "a2a_corpus_walk" || c.Status != "SKIPPED" {
		t.Fatalf("got %s %s, want a2a_corpus_walk SKIPPED", c.Name, c.Status)
	}
}

func TestA2ACorpusWalk_ReadErrorWarns_617(t *testing.T) {
	c, _ := corpusWalkCheck(t, nil, errors.New("db down"))
	if c.Status != "WARNING" || !strings.Contains(c.Message, "db down") {
		t.Fatalf("got %s %q", c.Status, c.Message)
	}
}

func TestA2ACorpusWalk_OKStatesWhatItExamined_617(t *testing.T) {
	c, since := corpusWalkCheck(t, []persistence.A2AKeyRetrieval{
		{KeyID: "k1", KeyName: "peer-a", Tasks: 21, Chunks: 196},
		{KeyID: "k2", Tasks: 1, Chunks: 3},
	}, nil)
	if c.Status != "OK" {
		t.Fatalf("status %s: %s", c.Status, c.Message)
	}
	for _, want := range []string{"2 caller key(s)", "22 A2A task(s)", "199 distinct chunk(s)", "500"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q lacks %q", c.Message, want)
		}
	}
	if d := time.Since(since); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Errorf("window starts %s ago, want 24h", d)
	}
}

func TestA2ACorpusWalk_NoTrafficIsAVisiblyEmptyOK_617(t *testing.T) {
	c, _ := corpusWalkCheck(t, nil, nil)
	if c.Status != "OK" || !strings.Contains(c.Message, "0 caller key(s)") {
		t.Fatalf("got %s %q", c.Status, c.Message)
	}
}

func TestA2ACorpusWalk_NamesOnlyTheKeyAboveTheThreshold_617(t *testing.T) {
	c, _ := corpusWalkCheck(t, []persistence.A2AKeyRetrieval{
		{KeyID: "k-walk", KeyName: "peer-walker", Tasks: 50, Chunks: 501},
		{KeyID: "k-edge", KeyName: "peer-edge", Tasks: 40, Chunks: 500},
		{KeyID: "k-gone", Tasks: 50, Chunks: 900},
	}, nil)
	if c.Status != "WARNING" {
		t.Fatalf("status %s: %s", c.Status, c.Message)
	}
	if len(c.Items) != 2 {
		t.Fatalf("items %v, want the two keys above 500", c.Items)
	}
	all := strings.Join(c.Items, "\n")
	for _, want := range []string{"k-walk", "peer-walker", "501", "k-gone", "900"} {
		if !strings.Contains(all, want) {
			t.Errorf("items %q lack %q", all, want)
		}
	}
	if strings.Contains(all, "k-edge") {
		t.Errorf("a key at exactly 500 was named: %q", all)
	}
}
