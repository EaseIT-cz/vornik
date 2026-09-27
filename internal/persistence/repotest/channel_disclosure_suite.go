package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunChannelDisclosureSuite exercises a ChannelDisclosureRepository against
// whichever backend the caller wires. Run from BOTH sqlite and postgres so a
// dialect divergence (TIMESTAMPTZ handling, ON CONFLICT syntax) can't reach
// production — `go test ./...` is sqlite-only.
//
// This table is the EU AI Act Art 99 evidence trail: it answers "prove you
// disclosed to this session". Unlike ChannelSessionRepository — whose SQLite
// implementation is a deliberate no-op stub because an in-memory cache is
// authoritative there — this repo MUST be durable on every backend. A no-op
// would silently destroy the evidence trail on sqlite deployments.
func RunChannelDisclosureSuite(t *testing.T, repo persistence.ChannelDisclosureRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("WasServed is false for an unknown pair", func(t *testing.T) {
		served, err := repo.WasServed(ctx, "telegram", "never-seen")
		if err != nil {
			t.Fatalf("WasServed: %v", err)
		}
		if served {
			t.Error("an unrecorded (channel, session) must report not-served")
		}
	})

	t.Run("MarkServed then WasServed round-trips", func(t *testing.T) {
		if err := repo.MarkServed(ctx, "telegram", "chat-1", "hash-a"); err != nil {
			t.Fatalf("MarkServed: %v", err)
		}
		served, err := repo.WasServed(ctx, "telegram", "chat-1")
		if err != nil {
			t.Fatalf("WasServed: %v", err)
		}
		if !served {
			t.Error("a recorded pair must report served")
		}
	})

	t.Run("scoped by channel", func(t *testing.T) {
		if err := repo.MarkServed(ctx, "slack", "shared-id", "hash-b"); err != nil {
			t.Fatalf("MarkServed: %v", err)
		}
		served, err := repo.WasServed(ctx, "web-chat", "shared-id")
		if err != nil {
			t.Fatalf("WasServed: %v", err)
		}
		if served {
			t.Error("the same session id on a different channel must be independent")
		}
	})

	// Concurrent turns in one session must not produce two rows or two
	// notices — the PK makes MarkServed idempotent.
	t.Run("MarkServed is idempotent", func(t *testing.T) {
		if err := repo.MarkServed(ctx, "telegram", "chat-dup", "hash-c"); err != nil {
			t.Fatalf("first MarkServed: %v", err)
		}
		if err := repo.MarkServed(ctx, "telegram", "chat-dup", "hash-c"); err != nil {
			t.Fatalf("second MarkServed must not error: %v", err)
		}

		entries, err := repo.ServedBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ServedBetween: %v", err)
		}
		var n int
		for _, e := range entries {
			if e.Channel == "telegram" && e.SessionID == "chat-dup" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("idempotent MarkServed produced %d rows, want 1", n)
		}
	})

	// The enforcement-response query from design §6.1 — an evidence trail
	// nobody can extract under time pressure is not much of a defence.
	t.Run("ServedBetween returns the evidence window with its hash", func(t *testing.T) {
		if err := repo.MarkServed(ctx, "email", "thread-ev", "hash-evidence"); err != nil {
			t.Fatalf("MarkServed: %v", err)
		}

		entries, err := repo.ServedBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ServedBetween: %v", err)
		}

		var found *persistence.ChannelDisclosure
		for i := range entries {
			if entries[i].Channel == "email" && entries[i].SessionID == "thread-ev" {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			t.Fatal("recorded disclosure absent from the evidence window")
		}
		if found.TextHash != "hash-evidence" {
			t.Errorf("TextHash = %q, want %q — the hash is what proves WHICH wording was served",
				found.TextHash, "hash-evidence")
		}
		if found.ServedAt.IsZero() {
			t.Error("ServedAt must be populated; an undated disclosure proves nothing")
		}
	})

	t.Run("ServedBetween excludes rows outside the window", func(t *testing.T) {
		if err := repo.MarkServed(ctx, "telegram", "chat-window", "hash-d"); err != nil {
			t.Fatalf("MarkServed: %v", err)
		}
		past := time.Now().Add(-48 * time.Hour)
		entries, err := repo.ServedBetween(ctx, past.Add(-time.Hour), past)
		if err != nil {
			t.Fatalf("ServedBetween: %v", err)
		}
		for _, e := range entries {
			if e.SessionID == "chat-window" {
				t.Error("a row served now must not appear in a window that closed 48h ago")
			}
		}
	})

	// The regulatory-record page's evidence summary (regulatory record design
	// §6, review F5): per (channel, wording version), how many notices were
	// served and when the first and last were, aggregated in SQL on BOTH
	// backends. A unique channel keeps this run's rows apart from other runs'.
	t.Run("SummaryBetween groups by channel and wording", func(t *testing.T) {
		ch := uniqueID("sumch")
		for _, sess := range []string{"s1", "s2"} {
			if err := repo.MarkServed(ctx, ch, sess, "hash-a"); err != nil {
				t.Fatalf("MarkServed: %v", err)
			}
		}
		if err := repo.MarkServed(ctx, ch, "s3", "hash-b"); err != nil {
			t.Fatalf("MarkServed: %v", err)
		}
		sum, err := repo.SummaryBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("SummaryBetween: %v", err)
		}
		got := map[string]persistence.ChannelDisclosureSummary{}
		for _, row := range sum {
			if row.Channel == ch {
				got[row.TextHash] = row
			}
		}
		if len(got) != 2 || got["hash-a"].Count != 2 || got["hash-b"].Count != 1 {
			t.Fatalf("want hash-a x2 and hash-b x1 for %s, got %+v", ch, got)
		}
		a := got["hash-a"]
		if a.FirstServed.IsZero() || a.LastServed.Before(a.FirstServed) {
			t.Errorf("first/last served not set sensibly: %+v", a)
		}
		empty, err := repo.SummaryBetween(ctx, time.Now().Add(-48*time.Hour), time.Now().Add(-47*time.Hour))
		if err != nil || empty == nil && err != nil {
			t.Fatalf("an empty window is an empty result, not an error: %v", err)
		}
	})
}
