package service

import (
	"testing"

	"vornik.io/vornik/internal/mcp"
)

// The service is the config loader's hand-off for the process-spawn law
// (S1b-2): it mints a stdio server's program from loaded config, and only a
// stdio server gets one.
func TestConfiguredMCPProgram(t *testing.T) {
	p := configuredMCPProgram(mcp.ServerConfig{Transport: "stdio", Command: "uvx", Args: []string{"srv", "--x"}})
	if p.Path() != "uvx" || len(p.Args()) != 2 || p.Args()[1] != "--x" {
		t.Fatalf("stdio program = %q %v", p.Path(), p.Args())
	}
	for _, tr := range []string{"sse", "streamable-http", ""} {
		if !configuredMCPProgram(mcp.ServerConfig{Transport: tr, Command: "uvx"}).IsZero() {
			t.Errorf("transport %q must carry no program", tr)
		}
	}
}

// The workspace root registered for the git kinds: an unset root registers
// nothing (git stays refused), and "/" refuses startup.
func TestRegisterSpawnWorkspaceRoot(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "")
	if err := registerSpawnWorkspaceRoot(""); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if err := registerSpawnWorkspaceRoot("/"); err == nil {
		t.Fatal("/ as the workspace root must refuse startup")
	}
	if err := registerSpawnWorkspaceRoot(t.TempDir()); err != nil {
		t.Fatalf("absolute: %v", err)
	}
}
