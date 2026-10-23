package subjectstore

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/secrets"
)

// GitHub #395 (found at review, #344 security follow-up, Art 17 redact design
// §15.11.4a F3): a failure to build the export detector withheld every
// transcript JSON column and told nobody why. It is now logged once, at ERROR,
// and the export still fails closed (chat-audit design §9.2.6).
func TestExportDetectorFailureIsLoggedOnceAndStillWithholds(t *testing.T) {
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	prevLogger := exportLogger.Load()
	SetExportLogger(&l)
	t.Cleanup(func() { exportLogger.Store(prevLogger) })

	builds := 0
	get := onceExportDetector(func() (secrets.Detector, error) {
		builds++
		return nil, errors.New("pattern failed to compile")
	})
	prev := exportDetectorFn
	exportDetectorFn = get
	t.Cleanup(func() { exportDetectorFn = prev })

	for i := 0; i < 3; i++ {
		if got := redactExportJSON("metadata", `{"reason":"x"}`); got != withheldNoScanner {
			t.Fatalf("export must fail closed without a detector, got %s", got)
		}
	}
	if builds != 1 {
		t.Errorf("built %d times, want once", builds)
	}
	out := buf.String()
	if strings.Count(out, `"level":"error"`) != 1 || !strings.Contains(out, "pattern failed to compile") ||
		!strings.Contains(out, "secret_scanner_unavailable") {
		t.Errorf("want exactly one ERROR naming the cause and the consequence, got:\n%s", out)
	}
}

// The default builder succeeds on the constant patterns and logs nothing.
func TestDefaultExportDetectorBuilds(t *testing.T) {
	if defaultExportDetector() == nil {
		t.Fatal("the default-pattern export detector failed to build")
	}
}
