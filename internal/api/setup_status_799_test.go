package api

// GitHub #799 (operations-API design §6.13): setup/status reports the
// restart generation now and the one read at start, or neither when either
// is unknown; the contract is 1.5.0 with both fields optional.
//
// Found at: review (vornik-cloud control-plane design 5.5), observed at
// integration (the real handler reports no generation).

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/opsapi"
	"vornik.io/vornik/internal/restartgen"
)

func status799(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.SetupStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil))
	return rec.Body.String()
}

func TestSetupStatus799_Generations(t *testing.T) {
	cfg := &config.Config{}
	gen := restartgen.New(filepath.Join(t.TempDir(), "config.yaml"))
	loaded, ok := gen.Read()
	s := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
	WithRestartGeneration(gen, loaded, ok)(s)

	if b := status799(t, s); !strings.Contains(b, `"pending_generation":0`) || !strings.Contains(b, `"loaded_generation":0`) {
		t.Fatalf("a fresh directory: %s", b)
	}
	// A mark after start: pending moves ahead of loaded.
	if err := gen.Bump(); err != nil {
		t.Fatal(err)
	}
	if b := status799(t, s); !strings.Contains(b, `"pending_generation":1`) || !strings.Contains(b, `"loaded_generation":0`) {
		t.Fatalf("after a mark: %s", b)
	}
	// A new process reading the same directory reports them equal.
	l2, ok2 := gen.Read()
	s2 := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
	WithRestartGeneration(gen, l2, ok2)(s2)
	if b := status799(t, s2); !strings.Contains(b, `"pending_generation":1`) || !strings.Contains(b, `"loaded_generation":1`) {
		t.Fatalf("after a restart: %s", b)
	}
}

func TestSetupStatus799_UnknownLeavesBothOut(t *testing.T) {
	cfg := &config.Config{}
	s := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
	WithRestartGeneration(restartgen.New(filepath.Join(t.TempDir(), "config.yaml")), 0, false)(s)
	if b := status799(t, s); strings.Contains(b, "generation") {
		t.Fatalf("an unknown start value still reported: %s", b)
	}
	bare := &Server{config: cfg, setupDetector: onboarding.Detector{Config: cfg}}
	if b := status799(t, bare); strings.Contains(b, "generation") {
		t.Fatalf("no counter wired, yet reported: %s", b)
	}
}

func TestOpsContract799_Generations(t *testing.T) {
	raw, ok := opsapi.Document("v1")
	if !ok {
		t.Fatal("no v1 document")
	}
	doc := string(raw)
	// The generations arrived in 1.5.0; a later 1.x keeps them.
	if !regexp.MustCompile(`(?m)^  version: 1\.([5-9]|[1-9][0-9]+)\.\d+$`).MatchString(doc) {
		t.Fatal("the contract is older than 1.5.0")
	}
	_, rest, _ := strings.Cut(doc, "/api/v1/setup/status:")
	status, _, _ := strings.Cut(rest, "\n  /api/")
	for _, f := range []string{"pending_generation:", "loaded_generation:", "required: [fresh_install]"} {
		if !strings.Contains(status, f) {
			t.Errorf("setup/status lacks %s", f)
		}
	}
}
