package agentloop

import (
	"os"
	"path/filepath"
	"testing"
)

// GitHub #612: a design that quotes a tool call's closing frame outside code
// would be refused when an agent writes it, so designs describe the frame in
// words, as TestDesignDocsCarryNoSecretShapedText_550 keeps secret-shaped
// text out of them.
func TestDesignDocsQuoteNoBareToolCallFrame_612(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "low-level-design")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// As TestDesignDocsCarryNoSecretShapedText_550: the directory exists
		// or it does not; an empty directory still fails below.
		t.Skip("no docs/low-level-design in this checkout (the CE export strips it)")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no designs found (%v)", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if frame, at := mergedToolCallFrame(string(b)); frame != "" {
			t.Errorf("%s quotes the frame %q outside code at byte %d; describe it in words", p, frame, at)
		}
	}
	t.Logf("examined %d designs", len(paths))
}
