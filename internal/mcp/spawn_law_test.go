package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// A stdio ServerConfig built anywhere but the config loader's hand-off carries
// no Program, and Connect refuses it before anything runs (process-spawn law,
// reading 3; S1b-2). This is the layer under the probe's own HTTP-only guard:
// a form or a probe that hand-builds ServerConfig{Command: <anything>} cannot
// start that command even if the guard above it regressed.
func TestConnect_StdioWithoutAConfiguredProgramIsRefused(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := ServerConfig{
		Name:      "from-a-form",
		Transport: "stdio",
		Command:   "/bin/sh",
		Args:      []string{"-c", "touch " + marker},
	}
	_, err := Connect(context.Background(), cfg, zerolog.Nop())
	if !errors.Is(err, ErrNoConfiguredProgram) {
		t.Fatalf("want ErrNoConfiguredProgram, got %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the command ran")
	}
}
