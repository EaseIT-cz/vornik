package docsmeta

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// #658: a brief in docs/architect/ anchors the sha256 of each design it
// cites. A design's front matter holds the hashes of ITS sources, so
// restamping a design after an unrelated source moved changed its bytes
// without changing a word of it, and every brief citing it read as stale
// (2026-11-06: 9 hash-only design restamps made 10 briefs stale).

func write658(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup658(t *testing.T) (root, design, brief string) {
	t.Helper()
	root = t.TempDir()
	design = filepath.Join(root, "docs/low-level-design/d.md")
	brief = filepath.Join(root, "docs/architect/b.md")
	write658(t, design, fmtDesign658("aaaa", "The body says one thing."))
	write658(t, brief, "---\nsources:\n    - path: docs/low-level-design/d.md\n      sha256: x\n---\n# Brief\n")
	if _, err := Restamp(root, brief); err != nil {
		t.Fatal(err)
	}
	return root, design, brief
}

func fmtDesign658(sha, body string) string {
	return "---\ndisclosure: customer\nsources:\n    - path: Makefile\n      sha256: " + sha + "\n---\n# Design\n\n" + body + "\n"
}

func TestStaleSources_658_StampOnlyDesignChangeLeavesBriefFresh(t *testing.T) {
	root, design, brief := setup658(t)
	// The design is restamped: only its own sources' hash line changes.
	write658(t, design, fmtDesign658("bbbb", "The body says one thing."))
	stale, err := StaleSources(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("a stamp-only change to a design must leave its brief fresh; got stale %v", stale)
	}
}

func TestStaleSources_658_DesignBodyChangeMarksBriefStale(t *testing.T) {
	root, design, brief := setup658(t)
	write658(t, design, fmtDesign658("aaaa", "The body now says another thing."))
	stale, err := StaleSources(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "docs/low-level-design/d.md" {
		t.Fatalf("a change to the design's text must mark the brief stale; got %v", stale)
	}
}

func TestStaleSources_899_LegacyWholeFileStampIsStale(t *testing.T) {
	// #899 retired the #658 fallback: a whole-file hash is no longer a valid
	// stamp, even of an unchanged design. The tree's legacy stamps were
	// migrated once (architect-expert design §5.8).
	root := t.TempDir()
	design := filepath.Join(root, "docs/low-level-design/d.md")
	content := fmtDesign658("aaaa", "Body.")
	write658(t, design, content)
	sum := sha256.Sum256([]byte(content))
	brief := filepath.Join(root, "docs/architect/b.md")
	write658(t, brief, "---\nsources:\n    - path: docs/low-level-design/d.md\n      sha256: "+hex.EncodeToString(sum[:])+"\n---\n# Brief\n")
	stale, err := StaleSources(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("a legacy whole-file stamp must read stale after #899; got %v", stale)
	}
}

// Review 878e F1: only the FRONT MATTER's sha256 lines leave the hash. A
// sha256 line in the design's body (an example, a code block) is text, and a
// change to it marks the brief stale.
func TestStaleSources_658_ASha256LineInTheBodyStaysHashed(t *testing.T) {
	root, design, brief := setup658(t)
	write658(t, design, fmtDesign658("aaaa", "The body says one thing."))
	if _, err := Restamp(root, brief); err != nil {
		t.Fatal(err)
	}
	write658(t, design, fmtDesign658("aaaa", "The body says one thing.\n\n```yaml\nsha256: one\n```"))
	if _, err := Restamp(root, brief); err != nil {
		t.Fatal(err)
	}
	write658(t, design, fmtDesign658("aaaa", "The body says one thing.\n\n```yaml\nsha256: two\n```"))
	stale, err := StaleSources(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("a sha256 line in the body is content; changing it must mark the brief stale, got %v", stale)
	}
}

// Review 878e F2: renaming a design's source changes its path line, which
// stays in the hash.
func TestStaleSources_658_ARenamedSourceMarksBriefStale(t *testing.T) {
	root, design, brief := setup658(t)
	write658(t, design, "---\ndisclosure: customer\nsources:\n    - path: go.mod\n      sha256: aaaa\n---\n# Design\n\nThe body says one thing.\n")
	stale, err := StaleSources(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("a renamed source is a change to the design; got stale %v", stale)
	}
}
