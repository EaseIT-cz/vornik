package configdrift

import (
	"os"
	"path/filepath"
	"testing"
)

// Drift design slice H (2026-10-03, review 508e F2): the installer now updates
// an untouched canonical file to the template and rewrites its .origin entry
// with mode "updated". A canonical comparison does not consult .origin, so the
// entry must change nothing the check reports: no finding while the file
// equals its template, and an ordinary divergence once the template moves on.
func TestCompare_UpdatedOriginEntryIsEvidence(t *testing.T) {
	f := newFixture(t)
	const rel = "role-library/coder.md"
	f.both(rel, "coder v2\n")
	f.write(".origin/"+rel, "coder v2\n")
	line := rel + "\t" + fixtureHash("coder v2\n") + "\trev-2\t2026-10-03T00:00:00Z\tupdated\n"
	if err := os.WriteFile(filepath.Join(f.root, ".origin/.index"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if ff := findFile(f.compare(), rel); ff != nil && (ff.CanonicalDiverged || len(ff.Missing) > 0) {
		t.Fatalf("an updated file equal to its template is reported: %+v", ff)
	}

	f.write(".templates/"+rel, "coder v3\n")
	ff := findFile(f.compare(), rel)
	if ff == nil || !ff.CanonicalDiverged {
		t.Fatalf("after the template moved on, finding %+v, want a canonical divergence", ff)
	}
}
