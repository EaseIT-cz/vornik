package a2a

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #640 (federation design §6, corrected 2026-11-05): the inbound A2A
// submit refuses a consult chain deeper than max_hops. Before the fix no
// inbound handler read the hop header, so a chain between two experts that
// can both consult was bounded only per task (8 calls), fanning out per hop.
//
// Found at: review (checking the A2A architect brief against the code), observed at integration (a published expert's real HTTP handler)
// Regression test at: integration, the real agent route over httptest.

func submitWithHops(t *testing.T, h *Handler, metadata string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"message":{"parts":[{"type":"text","text":"q"}]}` + metadata + `}`
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/demo/research/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.HandleAgentRoute(rec, req)
	return rec
}

func TestTaskSubmit_ConsultHopLimit_640(t *testing.T) {
	hops := func(v string) string { return `,"metadata":{"` + config.ConsultHopHeader + `":` + v + `}` }
	for _, tc := range []struct {
		name     string
		metadata string
		want     int
	}{
		{"no hop count is a first consult", "", http.StatusAccepted},
		{"at the limit", hops("2"), http.StatusAccepted},
		{"one past the limit", hops("3"), http.StatusTooManyRequests},
		{"far past the limit", hops("50"), http.StatusTooManyRequests},
		{"negative", hops("-1"), http.StatusUnprocessableEntity},
		{"fraction", hops("1.5"), http.StatusUnprocessableEntity},
		{"a string", hops(`"2"`), http.StatusUnprocessableEntity},
		{"absurd", hops("100000"), http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, creator := newTestHandler()
			h.MaxConsultHops = 2
			rec := submitWithHops(t, h, tc.metadata)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			created := creator.lastParams.Prompt != ""
			if (tc.want == http.StatusAccepted) != created {
				t.Fatalf("task created = %v for status %d", created, rec.Code)
			}
			if tc.want == http.StatusTooManyRequests && !strings.Contains(rec.Body.String(), "consult hop limit exceeded") {
				t.Fatalf("body %q", rec.Body.String())
			}
			// An accepted count is stored on the task, in context.a2a.metadata,
			// where the service's taskHopLookup reads it back (review 351b F1).
			if tc.name == "at the limit" {
				a2aCtx, _ := creator.lastParams.ExtraContext["a2a"].(map[string]any)
				md, _ := a2aCtx["metadata"].(map[string]any)
				if got, _ := md[config.ConsultHopHeader].(float64); got != 2 {
					t.Fatalf("stored hop count = %v, want 2", md[config.ConsultHopHeader])
				}
			}
		})
	}
}

// With no limit configured the daemon's default applies, never "unlimited".
func TestTaskSubmit_ConsultHopLimitDefault_640(t *testing.T) {
	h, _ := newTestHandler()
	over := config.DefaultConsultMaxHops + 1
	rec := submitWithHops(t, h, `,"metadata":{"`+config.ConsultHopHeader+`":`+itoa(over)+`}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d with the default limit %d and hops %d", rec.Code, config.DefaultConsultMaxHops, over)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
