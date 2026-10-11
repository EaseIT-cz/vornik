package mcp

import (
	"context"
	"encoding/json"
	"github.com/rs/zerolog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"vornik.io/vornik/internal/requestguard"
)

// #119 queued action: a refusing assembled-request guard must send nothing.
func TestPhysicalRequestGuard(t *testing.T) {
	for _, transport := range []string{"sse", "streamable-http"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			}))
			defer srv.Close()
			c := &Client{config: ServerConfig{Name: "mail", Transport: transport, URL: srv.URL, AuthHeaderProvider: func(context.Context) (map[string]string, error) {
				return map[string]string{"Authorization": "Carol"}, nil
			}}, httpClient: srv.Client(), logger: zerolog.Nop()}
			ctx := requestguard.With(context.Background(), func(context.Context) error { return &requestguard.Refusal{} })
			_, err := c.call(ctx, "tools/call", json.RawMessage(`{}`))
			if !requestguard.IsRefusal(err) || calls.Load() != 0 {
				t.Fatalf("stale assembled request: error=%v upstream=%d", err, calls.Load())
			}
		})
	}
}
