package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// vornikctl doctor ack posts to the daemon (which owns the write) and prints
// what was recorded and pruned.
func TestDoctorAck_PostsAndPrints(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/doctor/ack" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"file":"workflows/w.md","recorded":[{"class":"hard","key":"abcdef0123456789ffff","regime":"-"}],` +
			`"pruned":[{"rel":"workflows/x.md","class":"soft","key":"k","regime":"vague"}],"date":"2026-09-24T12:00:00Z"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	var out bytes.Buffer
	doctorAckCmd.SetOut(&out)
	if err := runDoctorAck(doctorAckCmd, []string{"config_template_drift", "workflows/w.md"}); err != nil {
		t.Fatal(err)
	}
	if got["check"] != "config_template_drift" || got["file"] != "workflows/w.md" {
		t.Errorf("body %v", got)
	}
	s := out.String()
	for _, want := range []string{"acknowledged 1 finding(s) for workflows/w.md", "hard", "abcdef0123456789", "pruned 1", "workflows/x.md"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
}

// A refusal from the daemon reaches the operator as an error.
func TestDoctorAck_RefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"NOTHING_TO_ACKNOWLEDGE","message":"config_template_drift has no finding for workflows/w.md"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	err := runDoctorAck(doctorAckCmd, []string{"config_template_drift", "workflows/w.md"})
	if err == nil || !strings.Contains(err.Error(), "no finding") {
		t.Fatalf("err %v", err)
	}
}
