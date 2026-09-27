package autonomy

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/spawn"
)

// s6ComposeDaemonGitConfig composes the daemon's git config from the
// operator's HOME and registers it, exactly as the daemon's startup does
// (process-spawn law S6-D4), restoring the previous registration afterwards.
// The host's system config is kept out of the test.
func s6ComposeDaemonGitConfig(t *testing.T, home string) {
	t.Helper()
	_ = home // HOME/XDG_CONFIG_HOME are already pointed at it
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	restore := spawn.RegisterGitGlobalConfig(spawn.GitGlobalConfig())
	t.Cleanup(restore)
	if _, err := spawn.ComposeGitConfig(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("compose the daemon's git config: %v", err)
	}
}
