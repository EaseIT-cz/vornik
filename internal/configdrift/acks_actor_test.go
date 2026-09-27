package configdrift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Slice F (drift design, 2026-09-25): an acknowledgement records WHO decided,
// not only what and when. Round 13 filed it: the endpoint authenticated an
// admin and the record carried only the date.

func driftFinding(t *testing.T) (*fixture, AckKey) {
	t.Helper()
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nfix one\nb\n")
	f.write("workflows/w.md", "a\nb\n")
	return f, hardKeyOf(t, f.compare(), "workflows/w.md")
}

func TestAckActor_RecordedInStoreAndJournalAndReadBack(t *testing.T) {
	f, k := driftFinding(t)
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "api_key_id:ops-1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ackStoreName, filepath.FromSlash(ackJournalPath)} {
		body, err := os.ReadFile(filepath.Join(f.root, p))
		if err != nil || !strings.Contains(string(body), "\tapi_key_id:ops-1\n") {
			t.Fatalf("%s must carry the actor as its sixth field: %q %v", p, body, err)
		}
	}
	a, err := ReadAcks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if st, rec := a.StatusRecord("workflows/w.md", k); st != Acked || rec.Actor != "api_key_id:ops-1" {
		t.Fatalf("StatusRecord = %v %+v; the pre-slice-F reader dropped the actor", st, rec)
	}
}

func TestAckActor_EmptyIsRefused(t *testing.T) {
	f, _ := driftFinding(t)
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, " \t"); err == nil {
		t.Fatal(`an ack without an actor must be refused: "" marks a pre-slice-F record`)
	}
}

// Both compatibility directions: a five-field (pre-F) line still suppresses and
// has no actor; a line with MORE fields than this binary knows still parses.
func TestAckActor_LenientParseBothDirections(t *testing.T) {
	f, k := driftFinding(t)
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "api_key_id:ops-1"); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(f.root, ackStoreName)
	body, _ := os.ReadFile(store)
	line := strings.TrimSuffix(string(body), "\n")
	parts := strings.Split(line, "\t")
	five := strings.Join(parts[:5], "\t") + "\n"
	if err := os.WriteFile(store, []byte(five), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := ReadAcks(f.root)
	if st, rec := a.StatusRecord("workflows/w.md", k); st != Acked || rec.Actor != "" {
		t.Fatalf("a five-field line must suppress with no actor: %v %+v", st, rec)
	}
	seven := strings.Join(parts[:6], "\t") + "\tfuture-field\n"
	if err := os.WriteFile(store, []byte(seven), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ = ReadAcks(f.root)
	if st, rec := a.StatusRecord("workflows/w.md", k); st != Acked || rec.Actor != "api_key_id:ops-1" {
		t.Fatalf("a seven-field line must still parse (the lenient-parse invariant): %v %+v", st, rec)
	}
}

// Two acks of one finding in the same second by different admins: the later
// journal line wins AND equals the store's actor (the pre-F reader kept the
// first line on an equal date, diverging from the store).
func TestAckActor_SameSecondLaterLineWinsAndMatchesTheStore(t *testing.T) {
	f, k := driftFinding(t)
	for _, who := range []string{"api_key_id:first", "session:second"} {
		if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, who); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := ReadAcks(f.root)
	_, stored := a.StatusRecord("workflows/w.md", k)
	journalRec := a.journal[AckRecord{Rel: "workflows/w.md", Class: k.Class, Key: k.Key, Regime: k.Regime}.id()]
	if stored.Actor != "session:second" || journalRec.Actor != "session:second" {
		t.Fatalf("store %q, journal %q; both must name the later actor", stored.Actor, journalRec.Actor)
	}
}

// A tab, a CR and an LF in an actor cannot split the record.
func TestAckActor_SeparatorsAreSanitised(t *testing.T) {
	f, k := driftFinding(t)
	if _, err := Acknowledge(f.root, "workflows/w.md", ackNow, nil, "session:a\tb\rc\nd"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(f.root, ackStoreName))
	if n := strings.Count(string(body), "\n"); n != 1 {
		t.Fatalf("one record must be one line, got %d:\n%q", n, body)
	}
	a, _ := ReadAcks(f.root)
	if _, rec := a.StatusRecord("workflows/w.md", k); rec.Actor != "session:a b c d" {
		t.Fatalf("actor = %q", rec.Actor)
	}
}

// Two vague journal records for one file on the same date: the pointer's actor
// is chosen deterministically, not by map order (review-20260925-e2f6 F1).
func TestAckActor_NowClassifiableTieIsDeterministic(t *testing.T) {
	a := &Acks{store: map[string]AckRecord{}, journal: map[string]AckRecord{}}
	for _, r := range []AckRecord{
		{Rel: "w.md", Class: AckSoft, Key: "k1", Regime: "vague", Date: "2026-09-25T10:00:00Z", Actor: "session:a"},
		{Rel: "w.md", Class: AckSoft, Key: "k2", Regime: "vague", Date: "2026-09-25T10:00:00Z", Actor: "session:b"},
	} {
		a.journal[r.id()] = r
	}
	first := ""
	for i := 0; i < 50; i++ {
		st, rec := a.StatusRecord("w.md", AckKey{Class: AckSoft, Key: "exact-k", Regime: "exact"})
		if st != NowClassifiable {
			t.Fatalf("status %v", st)
		}
		if first == "" {
			first = rec.Actor
		} else if rec.Actor != first {
			t.Fatalf("the tie picked %q then %q", first, rec.Actor)
		}
	}
}
