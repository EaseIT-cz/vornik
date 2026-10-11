package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #101 (2026-10-08): list_my_setup said a credential was "set" and
// an assistant read it as "works". Each connection now carries when it was
// last tested and the result, from the newest test_connection audit row
// (design §6.2), and says plainly when it never was.

func setupConnection(t *testing.T, v SetupView, name string) (server *SetupServer, api *SetupAPI) {
	t.Helper()
	for i := range v.Projects {
		for j := range v.Projects[i].Servers {
			if v.Projects[i].Servers[j].Name == name {
				return &v.Projects[i].Servers[j], nil
			}
		}
		for j := range v.Projects[i].APIs {
			if v.Projects[i].APIs[j].Name == name {
				return nil, &v.Projects[i].APIs[j]
			}
		}
	}
	t.Fatalf("list_my_setup has no connection %s", name)
	return nil, nil
}

func TestListMySetup_ShowsLastCheckedFromNewestAudit(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	ctx := context.Background()
	older := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	for _, row := range []struct {
		at   time.Time
		code string
	}{{newer, "auth_rejected"}, {older, "ok"}} {
		if err := f.c.repos.ToolAudit.Log(ctx, &persistence.ToolAuditEntry{ProjectID: "hermes--finance",
			TaskID: "companion:test_connection:fio", ExecutionID: "compex_x" + row.code, ToolName: "agentadmin:test_connection",
			ToolInput: "connection=fio", ToolOutput: "code=" + row.code + " http_status_class=4xx", Outcome: "error", CreatedAt: row.at}); err != nil {
			t.Fatal(err)
		}
	}
	// The companion endpoint's own row for a call does not count.
	if err := f.c.repos.ToolAudit.Log(ctx, &persistence.ToolAuditEntry{ProjectID: "hermes--finance", TaskID: "companion:akey_hermes",
		ExecutionID: "compex_y", ToolName: "mcp__plugin_vornik-companion_vornik__test_connection", ToolOutput: "status=ok", CreatedAt: newer.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, a := setupConnection(t, f.setup(), "fio")
	if a.LastResult != "auth_rejected" || a.LastCheckedAt == nil || !a.LastCheckedAt.Equal(newer) || a.CheckNote != "" {
		t.Fatalf("api: %+v", a)
	}
	// A real probe becomes the newest row.
	f.mustTestConn("finance", "fio")
	if _, a := setupConnection(t, f.setup(), "fio"); a.LastResult != string(agentadmin.ConnOK) {
		t.Fatalf("after a probe: %+v", a)
	}
}

func TestListMySetup_NeverCheckedSaysSo(t *testing.T) {
	f := newFinance(t)
	bank := newFakeMCP(t, "")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank", URL: bank.srv.URL}))
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	v := f.setup()
	s, _ := setupConnection(t, v, "bank")
	_, a := setupConnection(t, v, "fio")
	for _, got := range []struct {
		result, note string
		at           *time.Time
	}{{s.LastResult, s.CheckNote, s.LastCheckedAt}, {a.LastResult, a.CheckNote, a.LastCheckedAt}} {
		if got.result != "never" || got.at != nil {
			t.Fatalf("never tested reads %q at %v", got.result, got.at)
		}
		if !strings.Contains(got.note, "a stored credential is not proof the provider accepts it") || !strings.Contains(got.note, "test_connection") {
			t.Fatalf("note: %q", got.note)
		}
	}
}

// The assistants page reads the same view: each connection shows when it
// was last tested, and a set credential reads "stored", never "connected"
// or "working".
func TestAgentProjectView_LastCheckedAndStoredWording(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	v := agentProjectView(SetupProject{ID: "hermes--fin",
		Servers:     []SetupServer{{Name: "bank", Status: "approved", URL: "https://bank.example/mcp", connectionCheck: connectionCheck{LastResult: "never"}}},
		APIs:        []SetupAPI{{Name: "fio", Status: "approved", BaseURL: "https://api.fio.example", connectionCheck: connectionCheck{LastResult: "auth_rejected", LastCheckedAt: &at}}},
		Credentials: []SetupCredential{{Name: "FIO", Kind: "secret", Status: "set"}}})
	got := map[string]string{}
	for _, it := range v.Integrations {
		got[it.Name] = it.Checked
	}
	if got["bank"] != "not tested" || got["fio"] != "last tested 2026-10-08 09:30 UTC: auth_rejected" {
		t.Fatalf("checked: %v", got)
	}
	if len(v.Credentials) != 1 || v.Credentials[0].Status != "stored" {
		t.Fatalf("credential status: %+v", v.Credentials)
	}
}
