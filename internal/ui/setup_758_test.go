package ui

// The first run in three steps (installation-onboarding-guide-design.md,
// "Amendment 2026-11-14 (#758)", W2). Before #758 the page had a setup
// state block and four steps (chat, memory, dispatcher project, say hello),
// named config keys (telegram.dispatcher_project_id, config.yaml,
// secrets/chat.env) and told the user to restart the daemon from a shell.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

func renderSetup758(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d", rec.Code)
	}
	return rec.Body.String()
}

// setupVisibleText758 drops scripts, styles, tags and the operator's Setup details,
// leaving what a new user reads.
func setupVisibleText758(body string) string {
	if i := strings.Index(body, "Setup details"); i >= 0 {
		body = body[:i]
	}
	for _, re := range []string{`(?s)<script.*?</script>`, `(?s)<style.*?</style>`, `(?s)<[^>]+>`} {
		body = regexp.MustCompile(re).ReplaceAllString(body, " ")
	}
	return body
}

func TestSetup758_ThreeStepsInOrder(t *testing.T) {
	body := renderSetup758(t, NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}})))
	last := -1
	for _, step := range []string{"1. Connect a model", "2. Choose what it is for", "3. Say hello", "Long-term memory: optional, set up later", "Setup details"} {
		i := strings.Index(body, step)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order", step)
		}
		last = i
	}
	if !strings.Contains(body, `<details id="memory"`) {
		t.Error("memory is an optional, collapsed section after the first answer")
	}
}

func TestSetup758_NoConfigKeyNamesInWhatANewUserReads(t *testing.T) {
	text := setupVisibleText758(renderSetup758(t, NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))))
	// The memory section is collapsed and optional; check up to it.
	if i := strings.Index(text, "Long-term memory: optional"); i >= 0 {
		text = text[:i]
	}
	for _, key := range []string{"telegram.dispatcher_project_id", "config.yaml", "chat.env", "VORNIK_CHAT_API_KEY", "dispatcher project", "base URL"} {
		if strings.Contains(text, key) {
			t.Errorf("the first-run steps show %q", key)
		}
	}
}

func TestSetup758_UseCasesSayWhetherTheirProjectExists(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.Enabled, cfg.Chat.Provider, cfg.Chat.Endpoint, cfg.Chat.APIKey, cfg.Chat.Model = true, "http", "http://chat.example/v1", "sk-live", "gpt-live"
	body := renderSetup758(t, NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithProjectRegistry(newAssistantRegistry(t))))
	for _, want := range []string{
		`data-use-case="assistant" data-slug="personal-assistant" data-project="assistant" data-display="Personal Assistant" data-loaded="1"`,
		`data-use-case="companion" data-slug="companion" data-project="companion" data-display="Companion" data-loaded="0"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("use case markup missing %s", want)
		}
	}
	// The apply step waits for the daemon to go down and come back, and
	// falls back to the shell command after 300 s (design W1).
	for _, want := range []string{"waitForRestart", "300000", "120000", "NO_SUPERVISOR", "ALREADY_APPLYING", "JOB_RUNNING", "FILE_EXISTS", "AbortSignal.timeout", "systemctl --user restart vornik"} {
		if !strings.Contains(body, want) {
			t.Errorf("the apply script lacks %q", want)
		}
	}
}

// The page keeps two things the old dispatcher step offered (code review
// review-20261114-1de5 F1, F2): a link to connect a channel, and a way to
// make an existing assistant the dispatcher.
func TestSetup758_KeepsChannelsAndExistingAssistants(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.Enabled, cfg.Chat.Provider, cfg.Chat.Endpoint, cfg.Chat.APIKey, cfg.Chat.Model = true, "http", "http://chat.example/v1", "sk-live", "gpt-live"
	cfg.Telegram.DispatcherProjectID = "assistant"
	body := renderSetup758(t, NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}), WithProjectRegistry(newAssistantRegistry(t))))
	for _, want := range []string{`href="/ui/integrations"`, `id="existing-project"`, `<option value="assistant">Assistant</option>`, `data-use-case="existing"`, "Already set up on this install.", "Chosen: apply again"} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page lacks %s", want)
		}
	}
}
