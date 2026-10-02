package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/imageref"
)

// The legacy agent image rewrite is logged once, with the file and the fix
// (EaseIT-cz migration design §5.2).
func TestInstallLegacyImageLogger(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: zerolog.New(&buf)}
	c.installLegacyImageLogger()
	t.Cleanup(func() { imageref.SetLegacyImageLogger(nil) })
	imageref.ResetLegacyImageNotes()

	imageref.CanonicalFrom("ghcr.io/grinco/vornik-agent:latest", "swarms/x.md")
	imageref.CanonicalFrom("ghcr.io/grinco/vornik-agent:latest", "swarms/x.md")
	out := buf.String()
	if strings.Count(out, "legacy registry") != 1 || !strings.Contains(out, "swarms/x.md") || !strings.Contains(out, "sed -i") {
		t.Fatalf("want one warning naming the file and the fix, got:\n%s", out)
	}
}
