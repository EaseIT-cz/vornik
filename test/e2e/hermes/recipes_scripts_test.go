//go:build e2e_hermes

package hermes

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/test/standins"
)

// GitHub #404 (2026-10-17): A4's scripted model calls the mail and calendar
// stand-ins with fixed arguments, and A4 cannot tell an answered call from a
// refused one by the envelope it writes. This replays every scripted mail
// and calendar call against the stand-ins with the daemon's MCP client,
// without a container, so a script whose argument names the stand-in (and
// so the reference server) refuses fails before the lane runs. It runs with
// the lane's tag: go test -tags=e2e_hermes -run TestA4Scripts ./test/e2e/hermes/
func TestA4Scripts_TheStandInsAnswerEveryScriptedRead(t *testing.T) {
	servers := map[string]*standins.MCPServer{"gmail_": recipeMail(), "calendar_": recipeCalendar()}
	tokens := map[string]string{"gmail_": MailTokenCanary, "calendar_": CalendarTokenCanary}
	clients := map[string]*mcp.Client{}
	for prefix, s := range servers {
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := mcp.Connect(ctx, mcp.ServerConfig{Name: strings.TrimSuffix(prefix, "_"), Transport: "streamable-http", URL: srv.URL,
			AuthHeaders: map[string]string{"Authorization": "Bearer " + tokens[prefix]}}, zerolog.Nop())
		cancel()
		if err != nil {
			t.Fatalf("connect %s: %v", prefix, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		clients[prefix] = c
	}
	replayed := 0
	for _, sc := range recipeScripts() {
		for _, st := range sc.Steps {
			for prefix, c := range clients {
				if !strings.HasPrefix(st.ToolSuffix, prefix) {
					continue
				}
				raw, _ := json.Marshal(st.Args)
				res, err := c.CallTool(context.Background(), st.ToolSuffix, raw)
				if err != nil {
					t.Fatalf("%q: %s %s: %v", sc.Marker, st.ToolSuffix, raw, err)
				}
				if res.IsError {
					t.Errorf("%q: %s %s refused: %s", sc.Marker, st.ToolSuffix, raw, res.Text())
				}
				replayed++
			}
		}
	}
	// Denominator: digest (search, get), agenda (events), brief (events,
	// search, get).
	if replayed != 6 {
		t.Fatalf("replayed %d scripted reads, want 6", replayed)
	}
	for prefix, s := range servers {
		if n := len(s.Failed()); n != 0 {
			t.Errorf("%s stand-in recorded %d failed calls", prefix, n)
		}
	}
}
