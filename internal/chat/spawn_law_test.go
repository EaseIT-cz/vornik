package chat

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/spawn"
)

// The CLI providers run only a program the config loader's hand-off minted
// (process-spawn law, reading 3; S1b-2). A client built without one refuses
// to run rather than falling back to a program of its own choosing.
func TestCLIProviders_WithoutAConfiguredProgramRefuse(t *testing.T) {
	ctx := context.Background()
	if err := NewCLIClient("m").Ping(ctx); !errors.Is(err, spawn.ErrRefused) {
		t.Fatalf("claude Ping: want spawn.ErrRefused, got %v", err)
	}
	if _, err := NewCLIClient("m").Complete(ctx, []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, spawn.ErrRefused) {
		t.Fatalf("claude Complete: want spawn.ErrRefused, got %v", err)
	}
	if _, err := NewCodexCLIClient("m").Complete(ctx, []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, spawn.ErrRefused) {
		t.Fatalf("codex Complete: want spawn.ErrRefused, got %v", err)
	}
}
