package llmreplay

import (
	"strings"
	"testing"
)

// The index Load builds (audit 2026-09-05, BACKLOG "Remove discarded replay
// index construction"): Load used to build byHash twice and discard the
// first. These pin the index's observable contract across the cleanup —
// first entry wins on a duplicate hash, and every pointer lands in Entries
// even after enough appends to reallocate the slice.

func TestLoad_FirstEntryWinsOnADuplicateHash(t *testing.T) {
	req := `{"model":"m","messages":[{"role":"user","content":"same"}]}`
	rec := recordingOf(t,
		line(t, 1, req, `{"id":"first"}`, 0),
		line(t, 2, req, `{"id":"second"}`, 0))
	if len(rec.Entries) != 2 || len(rec.byHash) != 1 {
		t.Fatalf("entries=%d index=%d", len(rec.Entries), len(rec.byHash))
	}
	for _, e := range rec.byHash {
		if e.Seq != 1 {
			t.Fatalf("index points at seq %d, want the first entry", e.Seq)
		}
		if e != &rec.Entries[0] {
			t.Fatal("index entry is not the slice element — a stale pointer from before reallocation")
		}
	}
}

func TestLoad_EveryIndexPointerLandsInEntriesAfterReallocation(t *testing.T) {
	var lines []string
	for i := 1; i <= 200; i++ {
		req := `{"model":"m","messages":[{"role":"user","content":"q` + itoa(i) + `"}]}`
		lines = append(lines, line(t, i, req, `{"id":"r`+itoa(i)+`"}`, 0))
	}
	rec, err := Load(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.byHash) != 200 {
		t.Fatalf("index has %d entries, want 200", len(rec.byHash))
	}
	inSlice := map[*Entry]bool{}
	for i := range rec.Entries {
		inSlice[&rec.Entries[i]] = true
	}
	for h, e := range rec.byHash {
		if !inSlice[e] {
			t.Fatalf("index entry for %s points outside Entries", h)
		}
	}
}
