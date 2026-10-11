package mcp

import (
	"errors"
	"fmt"
	"testing"
)

// GitHub #92 (2026-10-08), live design Change C6: an MCP connect failure log
// line carries the HTTP status as a number, so the daemon-log ring can map it
// to a closed connection code without reading the error text.
func TestConnectFailureStatus(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", &CallError{Server: "s", Status: 401})
	for err, want := range map[error]int{
		wrapped:                   401,
		&CallError{Status: 503}:   503,
		errors.New("401 in text"): 0,
		nil:                       0,
	} {
		if got := connectFailureStatus(err); got != want {
			t.Errorf("connectFailureStatus(%v) = %d, want %d", err, got, want)
		}
	}
}
