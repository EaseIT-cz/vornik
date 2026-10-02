package service

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/secrets"
)

type countingExec struct{ n int }

func (c *countingExec) Tools(string) []chat.Tool { return nil }
func (c *countingExec) Execute(context.Context, string, string, string) (string, error) {
	c.n++
	return "ok", nil
}

// Plan P5.1: the daemon's egress scanner exists even with secrets.enabled
// off (that switch governs storage), detects for operator projects by
// default, and blocks for agent projects. Control: egressScanner.
func TestEgressScanner_DaemonPolicy(t *testing.T) {
	var logs bytes.Buffer
	c := &Container{Logger: zerolog.New(&logs), Config: &config.Config{}}
	scan := c.egressScanner()
	if scan == nil || scan.Detector == nil {
		t.Fatal("no egress scanner with secrets disabled")
	}
	if got := scan.Policy("assistant"); got != secrets.ActionDetect {
		t.Fatalf("operator default = %s, want detect", got)
	}
	ext := &countingExec{}
	scan.Record = c.recordEgress
	ce := &api.ComposedMCPExecutor{External: ext, Egress: scan}
	if _, err := ce.Execute(context.Background(), "hermes--fin", "mcp__mail__send", `{"k":"AKIAQWERTYUIOPASDFGH"}`); err == nil || !strings.Contains(err.Error(), "$.k") || ext.n != 0 {
		t.Fatalf("an agent call with a key: %v (%d sent)", err, ext.n)
	}
	if _, err := ce.Execute(context.Background(), "assistant", "mcp__mail__send", `{"k":"AKIAQWERTYUIOPASDFGH"}`); err != nil || ext.n != 1 {
		t.Fatalf("an operator call under detect: %v", err)
	}
	// The log line names type and path, never the value (review cb24).
	if strings.Contains(logs.String(), "AKIAQWERTYUIOPASDFGH") || !strings.Contains(logs.String(), "aws_access_key@$.k") {
		t.Fatalf("egress log: %s", logs.String())
	}
	c.Config.Secrets.Enabled = true
	c.Config.Secrets.Checkpoints = map[string]string{"tool_egress": "block"}
	c2 := &Container{Logger: zerolog.Nop(), Config: c.Config}
	if got := c2.egressScanner().Policy("assistant"); got != secrets.ActionBlock {
		t.Fatalf("configured operator action = %s, want block", got)
	}
}
