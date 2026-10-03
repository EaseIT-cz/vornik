package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// agent_namespace_budget (design §18.4 F10). Incident 2026-10-02: namespace
// claudecode summed $108 against a $104 ceiling and nothing reported it. The
// check names each namespace over its ceiling, the cover request waiting for
// it, and how many namespaces it examined, so "nothing over" is never read
// from a check that looked at nothing.
func TestCheckAgentNamespaceBudget(t *testing.T) {
	src := func(rows []AgentNamespaceBudgetStatus, err error) func(context.Context) ([]AgentNamespaceBudgetStatus, error) {
		return func(context.Context) ([]AgentNamespaceBudgetStatus, error) { return rows, err }
	}
	cases := []struct {
		name     string
		fn       func(context.Context) ([]AgentNamespaceBudgetStatus, error)
		status   string
		mentions []string
		absent   []string
	}{
		{"not wired", nil, "SKIPPED", []string{"not wired"}, nil},
		{"no namespaces", src(nil, nil), "OK", []string{"0 agent namespace(s) examined"}, nil},
		{"all within", src([]AgentNamespaceBudgetStatus{{Namespace: "hermes", TotalUSD: 10, CeilingUSD: 10}}, nil),
			"OK", []string{"1 agent namespace(s) examined", "none above"}, nil},
		{"over with a cover request waiting", src([]AgentNamespaceBudgetStatus{
			{Namespace: "hermes", TotalUSD: 4, CeilingUSD: 10},
			{Namespace: "claudecode", TotalUSD: 108, CeilingUSD: 104, CoverRequestID: "apr_0123456789abcdef"},
		}, nil), "WARNING", []string{"2 agent namespace(s) examined", "claudecode", "$108", "$104", "apr_0123456789abcdef"}, []string{"hermes"}},
		{"over with none waiting", src([]AgentNamespaceBudgetStatus{
			{Namespace: "claudecode", TotalUSD: 108, CeilingUSD: 104},
		}, nil), "WARNING", []string{"1 agent namespace(s) examined", "claudecode", "no cover request is waiting"}, []string{"apr_"}},
		{"source error", src(nil, errors.New("boom")), "WARNING", []string{"boom"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.fn != nil {
				h.SetAgentNamespaceBudgets(c.fn)
			}
			got := h.checkAgentNamespaceBudget(context.Background())
			if got.Name != "agent_namespace_budget" || got.Status != c.status {
				t.Fatalf("got %s %s %q, want %s", got.Name, got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Fatalf("message %q does not mention %q", got.Message, m)
				}
			}
			for _, m := range c.absent {
				if strings.Contains(got.Message, m) {
					t.Fatalf("message %q mentions %q", got.Message, m)
				}
			}
		})
	}
}
