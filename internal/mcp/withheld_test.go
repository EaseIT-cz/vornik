package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #91 (2026-10-08), auth design §14 (G1): a call against an oauth
// server withheld for want of a grant tells the model what is true and that
// varying the arguments will not help, instead of a bare "not connected".
func TestWithheldNoGrant_CallErrorNamesTheRemedy(t *testing.T) {
	m := NewManager(zerolog.Nop())
	remedy := "not connected, no grant: run vornikctl mcp connect atlassian -p assistant"
	m.SetWithheld(map[string]map[string]string{"assistant": {"atlassian": remedy}})
	for name, call := range map[string]func() error{
		"Execute": func() error {
			_, err := m.Execute(context.Background(), "assistant", "mcp__atlassian__search", "{}")
			return err
		},
		"CallToolOnce": func() error {
			_, _, err := m.CallToolOnce(context.Background(), "assistant", "mcp__atlassian__search", "{}")
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, want := range []string{`is configured for project "assistant" but ` + remedy, "varying its arguments will not help"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q lacks %q", name, err, want)
			}
		}
	}
	// A server that is simply unknown keeps the plain message.
	_, err := m.Execute(context.Background(), "assistant", "mcp__other__x", "{}")
	if err == nil || strings.Contains(err.Error(), "no grant") {
		t.Fatalf("an unknown server got the no-grant text: %v", err)
	}
}
