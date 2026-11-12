package service

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type recAudit625 struct {
	persistence.AdminAuditRepository
	rows []persistence.AdminAuditEntry
}

func (a *recAudit625) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, *e)
	return nil
}

// GitHub #625 item 2 (agent-administered design, #623 follow-up): the
// operator-credential audit is written once, by the service, for every
// caller, so the console page, the MCP form and the REST verb cannot drift.
// Found at: review. The row carries the caller's identity and never the value.
func TestOperatorCredentials_PutAndDeleteAuditOnce_625(t *testing.T) {
	audit := &recAudit625{}
	oc := &operatorCredentials{c: newCredContainer623(t), audit: audit}
	ctx := context.Background()
	who := persistence.AdminAuditEntry{Principal: "api_key_sha256:abc", Source: "api", IP: "10.0.0.9", UserAgent: "vornikctl"}
	if err := oc.Put(ctx, credName623, []byte("VALUE-CANARY"), who); err != nil {
		t.Fatal(err)
	}
	if err := oc.Delete(ctx, credName623, who); err != nil {
		t.Fatal(err)
	}
	if len(audit.rows) != 2 || audit.rows[0].Action != "credential.set" || audit.rows[1].Action != "credential.remove" {
		t.Fatalf("audit rows = %+v", audit.rows)
	}
	for _, r := range audit.rows {
		if r.Target != credName623 || r.Principal != who.Principal || r.Source != "api" || r.IP != "10.0.0.9" || r.UserAgent != "vornikctl" {
			t.Errorf("row %+v does not carry the caller", r)
		}
		if strings.Contains(r.Before+r.After+r.Target+r.Principal, "VALUE-CANARY") {
			t.Fatal("an audit row carries the value")
		}
	}
	// A nil repository skips the row; the write still succeeds.
	if err := (&operatorCredentials{c: newCredContainer623(t)}).Put(ctx, credName623, []byte("v"), who); err != nil {
		t.Fatalf("put without an audit repository: %v", err)
	}
}
