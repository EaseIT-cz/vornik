package cli

// GitHub #634 slice 2 (companion hardening design §13 H6, §13.10 S6):
// vornikctl companion hints -p <project> prints the operator report.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const hintReportFixture = `{"project":"proj","enabled":true,
 "hints":[{"id":"commit.review","trigger":"commit","state":"backed_off","backoff_until":"2026-11-19T12:00:00Z","window":"1000000000","follow_rate":0.1,"fired_total":10,"followed_total":1},
          {"id":"edits.recall_design","trigger":"edits","state":"inactive","window":"","fired_total":0,"followed_total":0}],
 "clients":[{"key_id":"k1","last_fetch_at":"2026-11-05T12:00:00Z"}]}`

func runHintsCmd(t *testing.T, jsonOut bool) (string, string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(hintReportFixture))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionHintsProject, companionHintsJSON = "proj", jsonOut
	var out bytes.Buffer
	companionHintsCmd.SetOut(&out)
	if err := runCompanionHints(companionHintsCmd, nil); err != nil {
		t.Fatal(err)
	}
	return gotPath, out.String()
}

func TestCompanionHintsCmd_Table_634(t *testing.T) {
	path, out := runHintsCmd(t, false)
	if path != "/api/v1/companion/hints/report?projectId=proj" {
		t.Fatalf("path = %q", path)
	}
	for _, want := range []string{"commit.review", "backed off until 2026-11-19", "1/10", "10%", "edits.recall_design", "inactive", "k1", "never"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCompanionHintsCmd_JSON_634(t *testing.T) {
	_, out := runHintsCmd(t, true)
	if !strings.Contains(out, `"backoff_until":"2026-11-19T12:00:00Z"`) {
		t.Fatalf("--json passes the report through: %s", out)
	}
}
