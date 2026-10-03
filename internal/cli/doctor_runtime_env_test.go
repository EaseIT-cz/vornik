package cli

import (
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
)

// D5 (onboarding-hardening-design.md, 2026-10-02): agent_image_uid reports
// what the agent WILL run as, so it needs runtime.run_as_user and the mount
// locations from the daemon config. Before D5 vornikctl passed only
// userns_mode, so the check could not see a configured run_as_user at all.
func TestRuntimeEnvFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runtime.RunAsUser = " 1001:1001 "
	cfg.Runtime.ProjectWorkspacePath = "/srv/vornik/workspaces"
	env := runtimeEnvFromConfig(cfg)
	if env.RunAsUser != "1001:1001" {
		t.Errorf("RunAsUser = %q, want 1001:1001", env.RunAsUser)
	}
	if env.ProjectWorkspacePath != "/srv/vornik/workspaces" {
		t.Errorf("ProjectWorkspacePath = %q", env.ProjectWorkspacePath)
	}
	// The ONE dependency-cache resolution the daemon and `vornikctl deps`
	// share (RuntimeConfig.DependencyCacheDir), not a second one.
	if want := filepath.Join("/srv/vornik", "deps"); env.DependencyCacheDir != want {
		t.Errorf("DependencyCacheDir = %q, want %q", env.DependencyCacheDir, want)
	}
}
