package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// GitHub #79 (operator 2026-10-04): every device showed as "Phone".
func TestPairDevice_DefaultLabelIsDistinct(t *testing.T) {
	ctx := context.Background()
	svc := newDeviceService(t)
	old := nowFunc
	t.Cleanup(func() { nowFunc = old })
	base := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		nowFunc = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		var out bytes.Buffer
		if err := pairDevice(ctx, svc, "https://vornik.example", false, "", &out); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		code := strings.TrimSpace(strings.SplitN(strings.SplitN(s, "Pairing code:", 2)[1], "\n", 2)[0])
		r, err := svc.Redeem(ctx, code, "10.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 { // revoke so the next pairing is a first-device pairing again
			if err := revokeDevice(ctx, svc, r.Device.ID, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	devs, err := svc.ListDevices(ctx)
	if err != nil || len(devs) != 2 {
		t.Fatalf("devices = %+v, %v", devs, err)
	}
	if devs[0].Label == devs[1].Label {
		t.Fatalf("both devices are labelled %q", devs[0].Label)
	}
	for _, d := range devs {
		if !strings.HasPrefix(d.Label, "Phone paired ") {
			t.Fatalf("label %q", d.Label)
		}
	}
	if got := defaultDeviceLabel(base); got != "Phone paired 4 Oct 09:30" {
		t.Fatalf("defaultDeviceLabel = %q", got)
	}
	if _, err := approverdevice.CleanLabel(defaultDeviceLabel(base)); err != nil {
		t.Fatal(err)
	}
}

// Final review aa4a, 2026-10-07 (T12 follow-up): the printed address must not
// send the operator to the "HTTPS required" refusal.
func TestPairDevice_NoOriginPrintsHTTPSPlaceholder(t *testing.T) {
	var out bytes.Buffer
	if err := pairDevice(context.Background(), newDeviceService(t), "", true, "P", &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "https://<this host>") || strings.Contains(s, "http://<this host>") {
		t.Fatalf("placeholder is not https:\n%s", s)
	}
	if strings.Contains(s, "needs HTTPS") {
		t.Fatalf("placeholder must not warn:\n%s", s)
	}
}

func TestPairDevice_PlainHTTPNonLoopbackWarns(t *testing.T) {
	for origin, warn := range map[string]bool{
		"http://192.168.0.142:8080": true,
		"http://vornik.lan":         true,
		"http://localhost:8080":     false,
		"http://127.0.0.1:8080":     false,
		"http://[::1]:8080":         false,
		"https://vornik.example":    false,
	} {
		var out bytes.Buffer
		if err := pairDevice(context.Background(), newDeviceService(t), origin, true, "P", &out); err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(out.String(), "pairing needs HTTPS")
		if got != warn {
			t.Errorf("origin %q: warning=%v want %v\n%s", origin, got, warn, out.String())
		}
	}
}
