package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `vornikctl doctor --json` dropped every field its own doctorCheck mirror did
// not declare. It decoded the daemon's response into that mirror and
// re-encoded it, so `kept` — added to api.DoctorCheck on 2026-09-24 so the
// cost-ledger rows orphan_fk_rows keeps are "visible to anything consuming
// vornikctl doctor --json" (orphan-FK ledger design F7) — never reached a
// consumer. Found on the reference host the same day: the daemon served
// kept=5166 and the CLI printed nothing. --json now passes the daemon's body
// through, so a field the daemon adds cannot be dropped by a CLI that has not
// heard of it yet.
func TestDoctorJSON_PassesEveryDaemonFieldThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"timestamp":"t","summary":"s","checks":[` +
			`{"name":"orphan_fk_rows","status":"OK","message":"m","kept":5166,"a_field_added_later":"x"}]}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	doctorJSON, doctorFix, doctorOffline = true, false, false
	defer func() { doctorJSON, doctorFix, doctorOffline = false, false, false }()

	out, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) })
	if err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("--json output lost its trailing newline (json.Encoder always wrote one)")
	}
	var got struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if len(got.Checks) != 1 {
		t.Fatalf("checks = %d, want 1:\n%s", len(got.Checks), out)
	}
	if got.Checks[0]["kept"] != float64(5166) {
		t.Errorf("kept dropped by the CLI: %v\n%s", got.Checks[0]["kept"], out)
	}
	if got.Checks[0]["a_field_added_later"] != "x" {
		t.Errorf("a field the CLI does not declare was dropped:\n%s", out)
	}
}
