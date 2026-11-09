package chat

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #458 (outcome-quality design §S8.9): WithModel built the model-pinned
// wrapper without the parent's CallStats, so a call made with a per-request
// model override was never tallied, and vornikctl doctor undercounted it.
func TestLoggingProvider_WithModelKeepsCallStats_458(t *testing.T) {
	stats := NewCallStats()
	stub := &logStub{model: "base", withModelTo: "yes", resp: &ChatResponse{}}
	p := NewLoggingProviderWithStats(stub, zerolog.New(&bytes.Buffer{}), stats)

	pinned := p.(ModelOverridable).WithModel("pinned")
	if _, err := pinned.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	var calls int
	for _, s := range stats.Snapshot() {
		if s.Model == "pinned" {
			calls += s.Calls
		}
	}
	if calls != 1 {
		t.Fatalf("a call through WithModel must be tallied under its model: got %d calls for \"pinned\" (snapshot %+v)", calls, stats.Snapshot())
	}
}
