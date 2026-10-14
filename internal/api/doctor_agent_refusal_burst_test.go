package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// GitHub #172 (2026-10-13): an assistant probing its boundaries was
// invisible. agent_refusal_burst warns on a namespace with 20 or more
// refusals in the last hour and states how many it examined (design §23.2).
func TestCheckAgentRefusalBurst(t *testing.T) {
	var since time.Time
	cases := []struct {
		name   string
		counts map[string]int
		err    error
		status string
		want   []string
	}{
		{"none", map[string]int{}, nil, "OK", []string{"0 agent namespace(s) with refusals examined, 0 refusal(s) in the last hour"}},
		{"below", map[string]int{"hermes": 19, "codex": 3}, nil, "OK", []string{"2 agent namespace(s) with refusals examined, 22 refusal(s)"}},
		{"burst", map[string]int{"hermes": 20, "codex": 3}, nil, "WARNING", []string{"hermes: 20 refusals", "2 agent namespace(s)"}},
		{"read error", nil, errors.New("db down"), "WARNING", []string{"could not read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			h.SetAgentRefusalCounts(func(_ context.Context, s time.Time) (map[string]int, error) {
				since = s
				return c.counts, c.err
			})
			got := h.checkAgentRefusalBurst(context.Background())
			if got.Name != "agent_refusal_burst" || got.Status != c.status {
				t.Fatalf("%+v, want status %s", got, c.status)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Message, w) {
					t.Errorf("message %q lacks %q", got.Message, w)
				}
			}
			if strings.Contains(got.Message, "codex: 3") {
				t.Errorf("a namespace below the threshold is named: %q", got.Message)
			}
		})
	}
	if d := time.Since(since); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("the window starts %v ago, want one hour", d)
	}
	if got := (&DoctorHandlers{}).checkAgentRefusalBurst(context.Background()); got.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", got)
	}
}
