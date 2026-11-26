package ui

// GitHub #766 (UI design §22): /ui/companion shows the owner the project's
// companion hints, read only, from the hints report the CLI reads. Before
// this the owner of a managed instance had no view of them (§16 census).
//
// Found at: review (the owner parity census, 2026-11-14), observed at
// integration (the page has no hints section).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/listener"
)

func TestCompanionPage766_TheHintsSection(t *testing.T) {
	rec := httptest.NewRecorder()
	companionServer(true, true).Handler().ServeHTTP(rec, companionPageRequest(auth.RoleTenantAdmin, listener.Public))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Companion hints", `id="companion-hints"`, `id="companion-hint-clients"`, "companion.mute"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

func TestCompanionPage766_TheScriptReadsTheReport(t *testing.T) {
	b, err := staticFS.ReadFile("static/companion.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"/api/v1/companion/hints/report?projectId=",
		"function renderHints(",
		"Hints are off for this project",
		"Hints are not available",
		"not shown yet",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("companion.js does not contain %q", want)
		}
	}
	// Built with textContent, never innerHTML, as the keys table is.
	start := strings.Index(js, "function renderHints(")
	if start < 0 {
		t.Fatal("no renderHints")
	}
	end := strings.Index(js[start:], "\n}\n")
	if end < 0 {
		end = len(js) - start
	}
	if strings.Contains(js[start:start+end], "innerHTML") {
		t.Error("renderHints uses innerHTML")
	}
}
