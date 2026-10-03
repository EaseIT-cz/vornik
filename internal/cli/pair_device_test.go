package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func newDeviceService(t *testing.T) *approverdevice.Service {
	t.Helper()
	db := sqlitetest.File(t, "d.db")
	return approverdevice.New(sqlite.NewApproverDeviceRepository(db.DB))
}

func TestPairDevice_PrintsCodeURLAndHints(t *testing.T) {
	ctx := context.Background()
	svc := newDeviceService(t)
	var out bytes.Buffer
	if err := pairDevice(ctx, svc, "https://vornik.example", false, "Pixel", &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "https://vornik.example/ui/pair") || !strings.Contains(s, "single use") {
		t.Fatalf("output:\n%s", s)
	}
	if strings.Contains(s, "already exists") {
		t.Fatal("first pairing claimed a device exists")
	}
	if !strings.Contains(s, "Warning: no alert channel") {
		t.Fatal("no push warning without a channel")
	}
	code := strings.TrimSpace(strings.SplitN(strings.SplitN(s, "Pairing code:", 2)[1], "\n", 2)[0])
	if len(code) != 9 || code[4] != '-' {
		t.Fatalf("code %q is not shown as XXXX-XXXX", code)
	}
	// The grouped form redeems: the dash is legibility only.
	if _, err := svc.Redeem(ctx, code, "10.0.0.1"); err != nil {
		t.Fatalf("the printed code does not redeem: %v", err)
	}
	out.Reset()
	if err := pairDevice(ctx, svc, "https://vornik.example", true, "Tablet", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already exists") || strings.Contains(out.String(), "Warning") {
		t.Fatalf("second pairing output:\n%s", out.String())
	}
}

func TestPairDevice_RefusesABadLabel(t *testing.T) {
	if err := pairDevice(context.Background(), newDeviceService(t), "", true, strings.Repeat("x", 41), &bytes.Buffer{}); err == nil {
		t.Fatal("a 41-character label was accepted")
	}
}

func TestDevices_ListAndRevoke(t *testing.T) {
	ctx := context.Background()
	svc := newDeviceService(t)
	code, _, _ := svc.StartPairing(ctx, "Pixel")
	r, err := svc.Redeem(ctx, code, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := listDevices(ctx, svc, true, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), r.DeviceToken) || strings.Contains(out.String(), approverdevice.HashToken(r.DeviceToken)) {
		t.Fatal("the list printed a token or its hash")
	}
	var rows []deviceListRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Label != "Pixel" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err := revokeDevice(ctx, svc, "dev_nope", &out); err == nil {
		t.Fatal("revoking an unknown id succeeded")
	}
	out.Reset()
	if err := revokeDevice(ctx, svc, rows[0].ID, &out); err != nil || !strings.Contains(out.String(), "Revoked") {
		t.Fatalf("revoke: %v %s", err, out.String())
	}
	out.Reset()
	_ = listDevices(ctx, svc, false, &out)
	if !strings.Contains(out.String(), "revoked") {
		t.Fatalf("table does not show the revocation:\n%s", out.String())
	}
	if _, err := svc.Authenticate(ctx, r.DeviceToken); err == nil {
		t.Fatal("a revoked device still authenticates")
	}
}

func TestPushConfigured(t *testing.T) {
	cfg := &config.Config{}
	if pushConfigured(cfg) {
		t.Fatal("empty config reports push")
	}
	cfg.SteeringNotificationsEnabled = true
	cfg.SteeringOperatorAlert.Channel = "telegram"
	if !pushConfigured(cfg) {
		t.Fatal("configured channel not reported")
	}
}
