package ui

// GitHub #623, OC4 (operator request, 2026-11-02): the MCP section of the
// control plane provisions the credential directly. A value typed beside a
// static reference is stored before the proposal is filed; the proposal
// carries only the reference; a failed store files no proposal; a server
// refused for a missing credential is listed as waiting for it. Design:
// 2026-10-02-agent-administered-vornik-design.md, Amendment 2026-11-02
// (#623), OC4. Regression test at: unit (the hub handler, as its existing
// tests drive it); the credentials page's integration test covers the store
// and the gate.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secretstore"
)

const hubValue623 = "hub-SECRET-token-never-in-a-proposal"

func hubForm623(valueFrom, value string) url.Values {
	return url.Values{
		"action": {"add"}, "name": {"Context7"}, "transport": {"streamable-http"},
		"url": {"https://mcp.context7.com/mcp"}, "auth_mode": {"static"},
		"auth_value_from": {valueFrom}, "auth_value_prefix": {"Bearer "},
		"auth_credential_value": {value},
	}
}

func hubRig623(t *testing.T, putErr error) (*Server, *storeCreds623, *recordingAudit623, func() int) {
	t.Helper()
	s, repo := mcpTestServer(t)
	db := sqlitetest.Memory(t)
	st, err := secretstore.New(sqlite.NewAgentSecretRepository(db.DB), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	audit := &recordingAudit623{}
	creds := &storeCreds623{st: st, env: map[string]string{}, referenced: map[string][]string{}, audit: audit}
	s.operatorCredentials = creds
	if putErr != nil {
		s.operatorCredentials = failingPut623{creds, putErr}
	}
	s.adminAuditRepo = audit
	count := func() int {
		ps, _ := repo.List(context.Background(), persistenceListAll())
		for _, p := range ps {
			if strings.Contains(p.Diff+p.ApplyContent+p.Rationale+p.Evidence+p.Title, hubValue623) {
				t.Fatalf("a proposal carries the credential value: %+v", p)
			}
		}
		return len(ps)
	}
	return s, creds, audit, count
}

type failingPut623 struct {
	*storeCreds623
	err error
}

func (f failingPut623) Put(context.Context, string, []byte, persistence.AdminAuditEntry) error {
	return f.err
}

func TestMCPHubCredential623_StoredThenProposedWithoutTheValue(t *testing.T) {
	s, creds, audit, proposals := hubRig623(t, nil)
	rec := postMCP(t, s, hubForm623("secret://CONTEXT7_API_KEY", hubValue623))
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "done=mcp-credential-saved") {
		t.Fatalf("want 303 mcp-credential-saved, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if v, err := creds.st.GetOperatorCredential(context.Background(), "CONTEXT7_API_KEY"); err != nil || string(v) != hubValue623 {
		t.Fatalf("credential not stored: (%q, %v)", v, err)
	}
	if n := proposals(); n != 1 {
		t.Fatalf("proposals = %d, want 1", n)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "credential.set" || audit.rows[0].Target != "CONTEXT7_API_KEY" {
		t.Errorf("audit = %+v", audit.rows)
	}
	if creds.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", creds.refreshes)
	}
	if strings.Contains(rec.Body.String()+rec.Header().Get("Location"), hubValue623) {
		t.Fatal("the response carries the value")
	}
}

func TestMCPHubCredential623_RefusalsFileNothing(t *testing.T) {
	for _, tc := range []struct {
		name, valueFrom, want string
		putErr                error
	}{
		{"lowercase name", "secret://context7-mcp", "mcp-credential-name", nil},
		{"namespaced name", "secret://hermes/CONTEXT7_API_KEY", "mcp-credential-name", nil},
		{"store fails", "secret://CONTEXT7_API_KEY", "mcp-credential-failed", errors.New("store down")},
		// Code review F3: an invalid server block stores nothing, even with a
		// valid credential name and value (the block validates first).
		{"invalid server block", "secret://CONTEXT7_API_KEY", "mcp-bad-transport", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, creds, audit, proposals := hubRig623(t, tc.putErr)
			form := hubForm623(tc.valueFrom, hubValue623)
			if tc.name == "invalid server block" {
				form.Set("transport", "carrier-pigeon")
			}
			rec := postMCP(t, s, form)
			if !strings.Contains(rec.Header().Get("Location"), "done="+tc.want) {
				t.Fatalf("want %s, got %d %s", tc.want, rec.Code, rec.Header().Get("Location"))
			}
			if n := proposals(); n != 0 {
				t.Errorf("a refused credential filed %d proposals", n)
			}
			if len(audit.rows) != 0 || creds.refreshes != 0 {
				t.Errorf("a refused credential audited %d rows, refreshed %d times", len(audit.rows), creds.refreshes)
			}
			if _, err := creds.st.GetOperatorCredential(context.Background(), "CONTEXT7_API_KEY"); err == nil {
				t.Error("a refused form stored the credential")
			}
		})
	}
}

func TestMCPHubCredential623_WaitingServersAreListed(t *testing.T) {
	s, creds, _, _ := hubRig623(t, nil)
	creds.referenced = map[string][]string{"CONTEXT7_API_KEY": {"Context7"}}
	req := setupAuthRequest(http.MethodGet, "/admin/control-plane?section=mcp", "admin")
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `data-waiting="CONTEXT7_API_KEY"`) || !strings.Contains(body, "Context7") {
		t.Fatalf("the waiting server is not listed (status %d)", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

func persistenceListAll() persistence.ProposalListFilter { return persistence.ProposalListFilter{} }
