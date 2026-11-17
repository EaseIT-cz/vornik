package cli

// GitHub #489 (design part 09 §9.2e): `vornikctl devices list` printed
// revoked devices forever; it now prints live ones and says how many it
// hid, and `devices remove` deletes a revoked device with an audit row.
// Found at: production (operator, 2026-10-21). Regression test at:
// integration (the CLI over the real store).

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type cliAudit489 struct {
	rows []*persistence.AdminAuditEntry
}

func (a *cliAudit489) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, e)
	return nil
}
func (a *cliAudit489) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

func TestDevices489_ListHidesRevokedAndRemoveDeletesIt(t *testing.T) {
	ctx := context.Background()
	svc := newDeviceService(t)
	audit := &cliAudit489{}
	svc.SetAudit(audit)
	code, _, _ := svc.StartPairing(ctx, "Pixel")
	if _, err := svc.Redeem(ctx, code, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	devs, _ := svc.ListDevices(ctx)
	id := devs[0].ID
	var out bytes.Buffer
	if err := removeDevice(ctx, svc, id, &out); err == nil || !strings.Contains(err.Error(), "revoke it first") {
		t.Fatalf("removing a live device: %v, want a refusal", err)
	}
	if err := svc.Revoke(ctx, id); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := listDevices(ctx, svc, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Pixel") || !strings.Contains(out.String(), "1 revoked device is hidden; --all shows it.") {
		t.Errorf("the default list should hide the revoked device and say so:\n%s", out.String())
	}
	out.Reset()
	_ = listDevices(ctx, svc, false, true, &out)
	if !strings.Contains(out.String(), "Pixel") {
		t.Errorf("--all should list the revoked device:\n%s", out.String())
	}
	out.Reset()
	if err := removeDevice(ctx, svc, id, &out); err != nil || !strings.Contains(out.String(), "Removed "+id+" (Pixel)") {
		t.Fatalf("remove a revoked device: %v %s", err, out.String())
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "device.remove" || audit.rows[0].Source != "vornikctl" ||
		!strings.HasPrefix(audit.rows[0].Principal, "cli:") || !strings.Contains(audit.rows[0].After, "Pixel") {
		t.Fatalf("want one device.remove row from the CLI keeping the label, got %+v", audit.rows)
	}
	if err := removeDevice(ctx, svc, id, &out); err == nil {
		t.Error("removing a removed device succeeded")
	}
}
