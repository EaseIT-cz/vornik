package benchlearning

// GitHub #890 (agent-benchmark design §12.37, part 1): the freeze names its
// database. Found at: production, the operator-run #809 2x2 study on
// 2026-11-23: arm B's freeze file outlived the recreated database, and arm C
// ran 7.5 hours of warm-up frozen, learning nothing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	armB = "database agentbench_g426b_1118 oid 16384"
	armC = "database agentbench_g426b_1118 oid 16999" // same name, recreated
)

func TestGate890_FreezeNamesItsDatabase(t *testing.T) {
	dir := t.TempDir()
	g, note, err := Open(dir, armB)
	if err != nil || note != "" {
		t.Fatalf("open: %v %q", err, note)
	}
	if _, err := g.Freeze(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FrozenFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || lines[1] != armB {
		t.Fatalf("marker = %q, want the time then %q", raw, armB)
	}
	// A restart on the same database stays frozen (a mid-pass restart).
	again, note, err := Open(dir, armB)
	if err != nil || note != "" {
		t.Fatalf("reopen: %v %q", err, note)
	}
	if _, frozen := again.FrozenAt(); !frozen {
		t.Fatal("the same database must stay frozen across a restart")
	}
}

// The incident: arm B's freeze must not freeze arm C's fresh database.
func TestGate890_AnotherDatabasesFreezeIsSetAside(t *testing.T) {
	dir := t.TempDir()
	g, _, _ := Open(dir, armB)
	if _, err := g.Freeze(); err != nil {
		t.Fatal(err)
	}
	c, note, err := Open(dir, armC)
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := c.FrozenAt(); frozen {
		t.Fatal("a freeze from another database froze a fresh arm (the #890 incident)")
	}
	for _, want := range []string{"oid 16384", "oid 16999", "set aside"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q should name %q", note, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FrozenFile)); !os.IsNotExist(err) {
		t.Fatalf("the foreign marker is still in place: %v", err)
	}
	stale := filepath.Join(dir, FrozenFile+".stale-16384")
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the foreign marker is kept for audit as %s: %v", stale, err)
	}

	// It stays set aside across a later restart, and a new freeze on this
	// database writes a fresh marker that holds.
	again, note, err := Open(dir, armC)
	if err != nil || note != "" {
		t.Fatalf("reopen: %v %q", err, note)
	}
	if _, frozen := again.FrozenAt(); frozen {
		t.Fatal("unfrozen arm C became frozen on restart")
	}
	if _, err := again.Freeze(); err != nil {
		t.Fatal(err)
	}
	if third, _, _ := Open(dir, armC); third == nil {
		t.Fatal("open failed")
	} else if _, frozen := third.FrozenAt(); !frozen {
		t.Fatal("arm C's own freeze must hold")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the audit copy must survive: %v", err)
	}
}

// A marker from before #890 names no database: it stays frozen (it cannot
// be told apart from this database's own freeze), with a note saying what to
// do before a new arm.
func TestGate890_AnOldMarkerStaysFrozenWithANote(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FrozenFile), []byte("2026-11-22T21:26:10.296012065Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, note, err := Open(dir, armC)
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := g.FrozenAt(); !frozen {
		t.Fatal("an old marker must stay frozen")
	}
	for _, want := range []string{filepath.Join(dir, FrozenFile), "names no database", "Delete it"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q should say %q", note, want)
		}
	}
}

func TestGate890_AnIdentityIsRequired(t *testing.T) {
	if _, _, err := Open(t.TempDir(), " "); err == nil {
		t.Fatal("a bench gate must know its database")
	}
}
