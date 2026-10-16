package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cpHTTPStub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestControlPlaneCommand_Wiring(t *testing.T) {
	sub := map[string]bool{}
	for _, c := range controlPlaneCmd.Commands() {
		sub[c.Name()] = true
	}
	for _, want := range []string{"proposals", "show", "approve", "reject", "propose", "diagnose"} {
		if !sub[want] {
			t.Errorf("control-plane missing subcommand %q: %v", want, sub)
		}
	}
	if controlPlaneCmd.Aliases[0] != "cp" {
		t.Errorf("expected alias cp, got %v", controlPlaneCmd.Aliases)
	}
}

func TestCPPropose_PostsCorrectly(t *testing.T) {
	var method, path string
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Title: "t", Status: "DRAFT"})
	})
	cpProposeKind, cpProposeScope, cpProposeTitle = "config", "project", "bump timeout"
	out, err := captureStdoutFn(t, func() error { return runCPPropose(nil, nil) })
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if method != http.MethodPost || path != "/api/v1/operator/proposals" {
		t.Errorf("wrong request: %s %s", method, path)
	}
	if body["kind"] != "config" || body["title"] != "bump timeout" {
		t.Errorf("wrong body: %v", body)
	}
	if !strings.Contains(out, "cpp_1") {
		t.Errorf("output missing id: %q", out)
	}
}

func TestCPDecide_PostsDecision(t *testing.T) {
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Status: "APPROVED", Approver: "vadim"})
	})
	cpActor = "vadim"
	out, err := captureStdoutFn(t, func() error { return runCPDecide("cpp_1", "approve") })
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if body["decision"] != "approve" || body["actor"] != "vadim" {
		t.Errorf("wrong decide body: %v", body)
	}
	if !strings.Contains(out, "APPROVED") {
		t.Errorf("output missing status: %q", out)
	}
}

func TestCPApply_PostsAck(t *testing.T) {
	var path string
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Status: "APPLIED"})
	})
	cpActor, cpApplyAck = "vadim", true
	out, err := captureStdoutFn(t, func() error { return runCPApply("cpp_1") })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if path != "/api/v1/operator/proposals/cpp_1/apply" {
		t.Errorf("path = %q", path)
	}
	if body["ackDaemon"] != true {
		t.Errorf("ackDaemon not sent: %v", body)
	}
	if !strings.Contains(out, "APPLIED") {
		t.Errorf("output: %q", out)
	}
}

func TestCPRollback_Posts(t *testing.T) {
	var path string
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Status: "ROLLED_BACK"})
	})
	out, err := captureStdoutFn(t, func() error { return runCPRollback("cpp_1") })
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if path != "/api/v1/operator/proposals/cpp_1/rollback" || !strings.Contains(out, "ROLLED_BACK") {
		t.Errorf("path=%q out=%q", path, out)
	}
}

func TestCPDiagnose_PostsFocusAndRendersVerdict(t *testing.T) {
	var path string
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"verdict": map[string]any{
				"root_cause":       "web_fetch timeout",
				"confidence":       "high",
				"evidence":         []string{"log line"},
				"suggested_change": "raise the scraper timeout",
			},
			"proposalId": "cpp_9",
		})
	})
	cpDiagnosePropose, cpDiagnoseJSON = true, false
	out, err := captureStdoutFn(t, func() error { return runCPDiagnose("family") })
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if path != "/api/v1/operator/diagnose" {
		t.Errorf("path = %q", path)
	}
	if body["focus"] != "family" || body["propose"] != true {
		t.Errorf("wrong body: %v", body)
	}
	if !strings.Contains(out, "web_fetch timeout") || !strings.Contains(out, "cpp_9") {
		t.Errorf("output missing verdict/proposal: %q", out)
	}
}

func TestCPDiagnose_NoProposalMessage(t *testing.T) {
	cpHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"verdict":    map[string]any{"root_cause": "unclear", "confidence": "low"},
			"proposalId": "",
		})
	})
	cpDiagnosePropose, cpDiagnoseJSON = true, false
	out, err := captureStdoutFn(t, func() error { return runCPDiagnose("family") })
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if !strings.Contains(out, "No proposal filed") {
		t.Errorf("expected no-proposal notice: %q", out)
	}
}

func TestCPProposals_ServerError(t *testing.T) {
	cpHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "NOT_FOUND", "message": "x"}})
	})
	if err := runCPShow("ghost"); err == nil {
		t.Fatal("expected error on 404")
	}
}

// GitHub #285 (2026-10-08): journal resolve --keep|--restore. Exactly one
// mode; the request carries it and the author.
func TestCPJournalResolve_PostsModeAndAuthor(t *testing.T) {
	var path string
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"journalId":"caj_1","mode":"keep","state":"APPLIED","proposalStatus":"APPLIED","changed":true,"affectedProjects":["hermes--global-news"]}`))
	})
	t.Cleanup(func() { cpJournalKeep, cpJournalRestore, cpActor = false, false, "" })
	cpJournalKeep, cpActor = true, "vadim"
	out, err := captureStdoutFn(t, func() error { return runCPJournalResolve("caj_1") })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if path != "/api/v1/operator/journal/caj_1/resolve" || body["mode"] != "keep" || body["actor"] != "vadim" {
		t.Fatalf("request: %s %v", path, body)
	}
	if !strings.Contains(out, "APPLIED") || !strings.Contains(out, "hermes--global-news") {
		t.Errorf("output: %q", out)
	}
}

// GitHub #301 (2026-10-08): keeping the files of a REJECTED proposal closes
// the row FAILED and leaves the proposal REJECTED; the CLI prints the
// daemon's plain sentence so "FAILED" is not read as the resolve failing.
func TestCPJournalResolve_PrintsTheMessage(t *testing.T) {
	cpHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"journalId":"caj_1","mode":"keep","state":"FAILED","proposalStatus":"REJECTED","changed":true,` +
			`"outcome":"kept_unapproved","message":"Kept the files as they are; the proposal stays REJECTED.","affectedProjects":["hermes--global-news"]}`))
	})
	t.Cleanup(func() { cpJournalKeep, cpJournalRestore, cpActor = false, false, "" })
	cpJournalKeep, cpActor = true, "vadim"
	out, err := captureStdoutFn(t, func() error { return runCPJournalResolve("caj_1") })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(out, "Kept the files as they are; the proposal stays REJECTED.") || !strings.Contains(out, "hermes--global-news") {
		t.Errorf("output: %q", out)
	}
}

func TestCPJournalResolve_ExactlyOneMode(t *testing.T) {
	t.Cleanup(func() { cpJournalKeep, cpJournalRestore = false, false })
	for _, c := range []struct{ keep, restore bool }{{false, false}, {true, true}} {
		cpJournalKeep, cpJournalRestore = c.keep, c.restore
		if err := runCPJournalResolve("caj_1"); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("keep=%v restore=%v: %v", c.keep, c.restore, err)
		}
	}
}

func TestCPJournalList_RendersOpenRows(t *testing.T) {
	cpHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rows":[{"id":"caj_1","proposalId":"cpp_1","state":"DRIFT","affectedProjects":["p1"],"failure":"drift: x"}],"count":1}`))
	})
	out, err := captureStdoutFn(t, func() error { return runCPJournalList(nil, nil) })
	if err != nil || !strings.Contains(out, "caj_1") || !strings.Contains(out, "DRIFT") || !strings.Contains(out, "p1") {
		t.Fatalf("list: %v %q", err, out)
	}
}
