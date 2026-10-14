package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/forge"
)

// audit 2026-10-12: GitHub #342 row 4 — the close-as-duplicate comment carried
// the agent's explanation signed only "Automated triage by Vornik.", with no
// Art 50 notice; every other forge publication carries the publication notice
// (Art 50 design §11.4).
func TestArt50_DuplicateCloseCommentCarriesNotice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice aidisclosure.Notice
	}{
		{"configured notice", aidisclosure.Notice{Text: "CUSTOM AI NOTICE"}},
		{"unset falls back to the compiled default", aidisclosure.Notice{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
					var v map[string]string
					_ = json.NewDecoder(r.Body).Decode(&v)
					posted = v["body"]
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/comments"):
					_, _ = w.Write([]byte(`[]`))
				case r.Method == http.MethodPatch:
					_, _ = w.Write([]byte(`{}`))
				default:
					n := 1
					if strings.HasSuffix(r.URL.Path, "/2") {
						n = 2
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "comments": 0})
				}
			}))
			defer srv.Close()
			p := testProvider(srv.URL)
			if err := p.CloseIssue(context.Background(), "o/r", 1, forge.CloseIssueSpec{
				DuplicateOf: 2, Explanation: "same defect", Notice: tc.notice}); err != nil {
				t.Fatal(err)
			}
			want := tc.notice
			if want.Text == "" {
				want = (*aidisclosure.Service)(nil).PublicationNotice()
			}
			if !strings.HasSuffix(posted, aidisclosure.FooterSeparator+want.Text) {
				t.Fatalf("duplicate comment = %q, want it to end with the AI notice %q", posted, want.Text)
			}
		})
	}
}
