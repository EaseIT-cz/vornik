package ui

// GitHub #846 (design 26 §3 C7): on a managed instance the owner mints a
// read-only key for AWS DevOps Agent from /ui/companion. The page shows the
// values the DevOps Agent form asks for; the script sends the read-only body.
//
// Found at: review (design 26, 2026-11-19), observed at integration (the page
// offered only a claude-code key with memory and skill writes and delegation).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/listener"
)

func TestCompanionPage846_TheDevOpsAgentStep(t *testing.T) {
	rec := httptest.NewRecorder()
	companionServer(true, true).Handler().ServeHTTP(rec, companionPageRequest(auth.RoleTenantAdmin, listener.Public))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Connect AWS DevOps Agent",
		`id="companion-create-aws"`,
		// The DevOps Agent form's three values: host, port, key.
		`<span class="font-mono text-gray-100">acme.vornik.cloud</span>`,
		`<span class="font-mono text-gray-100">443</span>`,
		"https://docs.vornik.io/guides/aws-devops-agent/",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

func TestCompanionPage846_TheReadOnlyBody(t *testing.T) {
	b, err := staticFS.ReadFile("static/companion.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	want := "projectId: 'companion', clientKind: 'aws-devops-agent', sessionLabel: 'aws-devops-agent ' + day(), " +
		"memoryRead: true, skillRead: true, delegateDisabled: true"
	if !strings.Contains(strings.Join(strings.Fields(js), " "), want) {
		t.Errorf("companion.js does not send the read-only body:\n%s", want)
	}
	// The read-only body grants no write scope.
	start := strings.Index(js, "clientKind: 'aws-devops-agent'")
	if start < 0 {
		t.Fatal("no aws-devops-agent body")
	}
	end := strings.Index(js[start:], "}")
	for _, refused := range []string{"memoryWrite", "skillWrite", "skillAdmin", "budgetCapUsd"} {
		if strings.Contains(js[start:start+end], refused) {
			t.Errorf("the read-only body carries %s", refused)
		}
	}
}

// The key table shows each key's grants (operator, 2026-11-19: the owner could
// not see what a key may do).
func TestCompanionPage846_TheGrantsColumn(t *testing.T) {
	b, err := staticFS.ReadFile("static/companion.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"function grants(k)", "k.memoryWrite", "k.skillAdmin", "k.delegateDisabled", "tr.appendChild(cell(grants(k)))"} {
		if !strings.Contains(js, want) {
			t.Errorf("companion.js does not contain %q", want)
		}
	}
	rec := httptest.NewRecorder()
	companionServer(true, true).Handler().ServeHTTP(rec, companionPageRequest(auth.RoleTenantAdmin, listener.Public))
	if !strings.Contains(rec.Body.String(), "<th>Can</th>") {
		t.Error("the key table has no grants column")
	}
}

// The step needs the instance's host name, from the same base the page's
// address comes from.
func TestCompanionPage846_HostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://acme.vornik.cloud":       "acme.vornik.cloud",
		"https://acme.vornik.cloud:8443/": "acme.vornik.cloud",
		"http://localhost:8080":           "localhost",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
	_ = http.MethodGet
}
