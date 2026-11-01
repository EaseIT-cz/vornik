package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #578 B4: the same check counts task credentials stored unsealed.
func TestDoctorTokensSealed578_CountsTaskCredentials(t *testing.T) {
	db := sqlitetest.Memory(t)
	h := NewDoctorHandlers(db.DB)
	ctx := context.Background()
	ins := func(id, value string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO task_credentials (id, task_id, execution_id, tool, label, value, artifact_url, created_at)
			VALUES (?, 't', 'e', ?, 'l', ?, '', '2026-10-31T00:00:00Z')`, id, id, value); err != nil {
			t.Fatal(err)
		}
	}
	ins("sealed", "sv1:AAAA")
	if c := h.checkMCPOAuthTokensSealed(ctx); c.Status != "OK" {
		t.Fatalf("only a sealed credential: %+v", c)
	}
	ins("plain", "hunter2-xY9pQ")
	c := h.checkMCPOAuthTokensSealed(ctx)
	if c.Status != "ERROR" || !strings.Contains(c.Message, "1 of 2 task credential(s)") {
		t.Fatalf("one unsealed credential: %+v", c)
	}
	if strings.Contains(c.Message, "hunter2") {
		t.Fatalf("the check must never print a value: %q", c.Message)
	}
}
