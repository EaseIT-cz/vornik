package api

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #571 A5: the doctor reports OK on a clean table and an ERROR naming
// the count when a grant holds an unsealed token.
func TestDoctorTokensSealed571(t *testing.T) {
	db := sqlitetest.Memory(t)
	h := NewDoctorHandlers(db.DB)
	ctx := context.Background()
	if c := h.checkMCPOAuthTokensSealed(ctx); c.Status != "OK" {
		t.Fatalf("empty table: %+v", c)
	}
	ins := func(project, access, refresh string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO mcp_oauth_tokens (project_id, server_name, resource, client_id, access_token, refresh_token, connected_at, updated_at)
			VALUES (?, 's', 'r', 'c', ?, ?, '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z')`, project, access, refresh); err != nil {
			t.Fatal(err)
		}
	}
	ins("sealed", "sv1:AAAA", "")
	if c := h.checkMCPOAuthTokensSealed(ctx); c.Status != "OK" {
		t.Fatalf("only sealed rows: %+v", c)
	}
	ins("plain", "AT-plain", "sv1:BBBB")
	ins("half", "sv1:CCCC", "RT-plain")
	c := h.checkMCPOAuthTokensSealed(ctx)
	if c.Status != "ERROR" || c.Message[:1] != "2" {
		t.Fatalf("two unsealed rows: %+v", c)
	}
}
