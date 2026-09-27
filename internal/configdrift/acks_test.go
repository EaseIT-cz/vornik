package configdrift

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var ackNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func (f *fixture) ack(rel string) *AckResult {
	f.t.Helper()
	res, err := Acknowledge(f.root, rel, ackNow, nil, "api_key_id:test")
	if err != nil {
		f.t.Fatalf("Acknowledge(%s): %v", rel, err)
	}
	return res
}

func (f *fixture) status(rel string, k AckKey) AckStatus {
	f.t.Helper()
	a, err := ReadAcks(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	st, _ := a.Status(rel, k)
	return st
}

func hardKeyOf(t *testing.T, r *Report, rel string) AckKey {
	t.Helper()
	ff := findFile(r, rel)
	if ff == nil {
		t.Fatalf("no finding for %s", rel)
	}
	for _, k := range ff.Keys() {
		if k.Class == AckHard {
			return k
		}
	}
	t.Fatalf("no hard key for %s", rel)
	return AckKey{}
}

// An ack silences exactly what it recorded, and a NEW template fix to the same
// file (a new hunk, a new key) re-opens it.
func TestAcknowledge_SilencesThenANewFixReopens(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nfix one\nb\n")
	f.write("workflows/w.md", "a\nb\n")
	k1 := hardKeyOf(t, f.compare(), "workflows/w.md")
	f.ack("workflows/w.md")
	if f.status("workflows/w.md", k1) != Acked {
		t.Fatal("the acked finding is not suppressed")
	}
	f.write(".templates/workflows/w.md", "a\nfix one\nb\nc\nfix two\n")
	f.write("workflows/w.md", "a\nb\nc\n")
	for _, k := range findFile(f.compare(), "workflows/w.md").Keys() {
		if k.Class == AckHard && k != k1 && f.status("workflows/w.md", k) == Acked {
			t.Fatal("a new template fix inherited the old acknowledgement")
		}
	}
}

// Matrix case 3: a soft ack re-keys when a new removal appears.
func TestAcknowledge_SoftReKeysOnANewRemoval(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\ntuned: 1\n")
	f.ack("workflows/w.md")
	f.write("workflows/w.md", "a\ntuned: 1\nmore: 2\n")
	k := softKey(t, findFile(f.compare(), "workflows/w.md"))
	if f.status("workflows/w.md", k) == Acked {
		t.Fatal("a changed soft set inherited the acknowledgement")
	}
}

// Matrix case 9: acked in the vague regime, then .origin appears — the
// finding re-opens as "now classifiable", not as a new problem.
func TestAcknowledge_VagueThenExactIsNowClassifiable(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nmax_attempts: 3\n")
	f.ack("workflows/w.md")
	f.origin("a\nmax_attempts: 3\n", "r0", "seeded")
	k := softKey(t, findFile(f.compare(), "workflows/w.md"))
	if st := f.status("workflows/w.md", k); st != NowClassifiable {
		t.Fatalf("status %v, want NowClassifiable", st)
	}
}

// Matrix case 12: an ack lost from the store (a restore) but present in the
// journal reads as "acknowledgement missing"; re-acking restores it.
func TestAcknowledge_StoreLostJournalKeepsItAndReackRestores(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nfix\n")
	f.write("workflows/w.md", "a\n")
	k := hardKeyOf(t, f.compare(), "workflows/w.md")
	f.ack("workflows/w.md")
	if err := os.Remove(filepath.Join(f.root, ackStoreName)); err != nil {
		t.Fatal(err)
	}
	if st := f.status("workflows/w.md", k); st != AckMissing {
		t.Fatalf("status %v, want AckMissing", st)
	}
	f.ack("workflows/w.md") // idempotent, and allowed although "acknowledged"
	if f.status("workflows/w.md", k) != Acked {
		t.Fatal("re-acking did not restore the store")
	}
}

// Matrix case 16: a canonical override re-opens when EITHER side changes.
func TestAcknowledge_CanonicalReopensOnEitherSide(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/pricing.yaml", "m: 1\n")
	f.write("pricing.yaml", "m: 2\n")
	f.ack("pricing.yaml")
	key := func() AckKey { return findFile(f.compare(), "pricing.yaml").Keys()[0] }
	if f.status("pricing.yaml", key()) != Acked {
		t.Fatal("canonical ack not applied")
	}
	f.write("pricing.yaml", "m: 3\n")
	if f.status("pricing.yaml", key()) == Acked {
		t.Fatal("a changed override inherited the ack")
	}
	f.ack("pricing.yaml")
	f.write(".templates/pricing.yaml", "m: 9\n")
	if f.status("pricing.yaml", key()) == Acked {
		t.Fatal("a changed template inherited the ack")
	}
}

// Round 8 N1: a removed-template ack re-opens when a later revision removes
// the path again.
func TestAcknowledge_RemovedReopensOnReRemoval(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/.removed", "workflows/old.md\tr1\t2026-09-01T00:00:00Z\n")
	f.write("workflows/old.md", "x\n")
	f.ack("workflows/old.md")
	if f.status("workflows/old.md", Removed{Rel: "workflows/old.md", Revision: "r1"}.Key()) != Acked {
		t.Fatal("removed ack not applied")
	}
	f.write(".templates/.removed", "workflows/old.md\tr7\t2026-09-20T00:00:00Z\n")
	if f.status("workflows/old.md", Removed{Rel: "workflows/old.md", Revision: "r7"}.Key()) == Acked {
		t.Fatal("a re-removal in a later revision inherited the ack")
	}
}

func TestAcknowledge_Refusals(t *testing.T) {
	f := newFixture(t)
	f.both("workflows/w.md", "same\n")
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "api_key_id:test"); !errors.Is(err, ErrNothingToAck) {
		t.Errorf("a file with no finding: %v, want ErrNothingToAck", err)
	}
	f.write(".templates/workflows/w.md", "same\nfix\n")
	f.remove(".templates/.stamp")
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "api_key_id:test"); !errors.Is(err, ErrBaselineNotCurrent) {
		t.Errorf("an unstamped baseline: %v, want ErrBaselineNotCurrent", err)
	}
	if _, err := Acknowledge(t.TempDir(), "workflows/w.md", ackNow, nil, "api_key_id:test"); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("no baseline at all: %v, want ErrNoBaseline", err)
	}
}

// Pruning happens at ack time: a record whose finding disappeared is dropped
// from the store, and the journal keeps it.
func TestAcknowledge_PrunesAtAckTimeJournalKeeps(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/a.md", "a\nfix\n")
	f.write("workflows/a.md", "a\n")
	f.write(".templates/workflows/b.md", "b\nfix\n")
	f.write("workflows/b.md", "b\n")
	f.ack("workflows/a.md")
	f.write("workflows/a.md", "a\nfix\n") // a's finding is gone (the fix was taken)
	res := f.ack("workflows/b.md")
	if len(res.Pruned) != 1 || res.Pruned[0].Rel != "workflows/a.md" {
		t.Fatalf("pruned %+v, want a.md's record", res.Pruned)
	}
	j, _ := os.ReadFile(filepath.Join(f.root, ackJournalPath))
	if n := len(splitLines(j)); n != 2 {
		t.Fatalf("journal has %d lines, want both acks kept", n)
	}
}

// Two concurrent acks of different files both land in the store.
func TestAcknowledge_ConcurrentAcksBothLand(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		f.write(".templates/workflows/"+n+".md", n+"\nfix\n")
		f.write("workflows/"+n+".md", n+"\n")
	}
	var wg sync.WaitGroup
	for _, n := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(rel string) {
			defer wg.Done()
			if _, err := Acknowledge(f.root, rel, ackNow, nil, "api_key_id:test"); err != nil {
				t.Error(err)
			}
		}("workflows/" + n + ".md")
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(f.root, ackStoreName))
	if n := len(splitLines(b)); n != 4 {
		t.Fatalf("store has %d records after 4 concurrent acks, want 4", n)
	}
}

// Round 11 F2: the reverse transition is NOT "now classifiable". Acked in the
// exact regime, then .origin is lost: the finding falls through.
func TestAcknowledge_ExactThenVagueIsNotNowClassifiable(t *testing.T) {
	f := newFixture(t)
	f.origin("a\nmax_attempts: 3\n", "r0", "seeded")
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nmax_attempts: 3\n")
	f.ack("workflows/w.md")
	f.remove(".origin/workflows/w.md") // .origin lost: the file degrades to vague
	k := softKey(t, findFile(f.compare(), "workflows/w.md"))
	if k.Regime != "vague" {
		t.Fatalf("regime %q, want vague after .origin was lost", k.Regime)
	}
	if st := f.status("workflows/w.md", k); st == NowClassifiable || st == Acked {
		t.Fatalf("status %v: an exact ack must not cover, or relabel, the unclassified form", st)
	}
}

// Round 11 F1: a journal write that fails leaves the store untouched — the
// journal is written first.
func TestAcknowledge_JournalFailureChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nfix\n")
	f.write("workflows/w.md", "a\n")
	// Make the journal unwritable: a DIRECTORY where the file must go.
	if err := os.MkdirAll(filepath.Join(f.root, ackJournalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "api_key_id:test"); err == nil {
		t.Fatal("the ack succeeded without its journal line")
	}
	if _, err := os.Stat(filepath.Join(f.root, ackStoreName)); !os.IsNotExist(err) {
		t.Fatalf("the store was written although the journal failed: %v", err)
	}
}

// Round 12 F2: the caller's currency test runs inside the lock, on the same
// comparison as the write, and a failing one refuses.
func TestAcknowledge_CurrencyTestedInsideTheLock(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nfix\n")
	f.write("workflows/w.md", "a\n")
	called := false
	_, err := Acknowledge(f.root, "workflows/w.md", ackNow, func(r *Report) (bool, string) {
		called = true
		if r == nil || r.Stamp != "rev-current" {
			t.Errorf("predicate saw report %+v", r)
		}
		return false, "stale (recorded by rev-current; this daemon is rev-next)"
	}, "api_key_id:test")
	if !called || !errors.Is(err, ErrBaselineNotCurrent) {
		t.Fatalf("called %v err %v", called, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, ackStoreName)); !os.IsNotExist(err) {
		t.Fatal("a refused ack wrote the store")
	}
}

// Round 12 F4: "now classifiable" survives the prune an unrelated ack performs.
func TestAcknowledge_NowClassifiableSurvivesAnUnrelatedPrune(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nmax_attempts: 3\n")
	f.write(".templates/workflows/other.md", "o\nfix\n")
	f.write("workflows/other.md", "o\n")
	f.ack("workflows/w.md") // vague
	f.origin("a\nmax_attempts: 3\n", "r0", "seeded")
	f.ack("workflows/other.md") // prunes w.md's now-stale vague record from the store
	k := softKey(t, findFile(f.compare(), "workflows/w.md"))
	if st := f.status("workflows/w.md", k); st != NowClassifiable {
		t.Fatalf("status %v after an unrelated prune, want NowClassifiable", st)
	}
}

// Round 13 N1: an EXACT ack whose store record was lost, for a file also acked
// vaguely earlier, reads as "acknowledgement missing" — not as "now
// classifiable" pointing at the older vague ack.
func TestAcknowledge_JournalAheadOfStoreIsAckMissingNotNowClassifiable(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nmax_attempts: 3\n")
	f.ack("workflows/w.md") // vague
	f.origin("a\nmax_attempts: 3\n", "r0", "seeded")
	f.ack("workflows/w.md") // exact
	if err := os.Remove(filepath.Join(f.root, ackStoreName)); err != nil {
		t.Fatal(err)
	}
	k := softKey(t, findFile(f.compare(), "workflows/w.md"))
	if st := f.status("workflows/w.md", k); st != AckMissing {
		t.Fatalf("status %v, want AckMissing", st)
	}
}
