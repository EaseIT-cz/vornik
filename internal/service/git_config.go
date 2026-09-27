package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/spawn"
)

// gitConfigDataDir is where the daemon's composed git config lives: under the
// data directory when there is one (so a bench daemon and production never
// share it), else a per-user directory under the temp dir.
func gitConfigDataDir() string {
	if dataDir := strings.TrimSpace(os.Getenv("VORNIK_DATA_DIR")); dataDir != "" {
		return dataDir
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("vornik-%d", os.Getuid()))
}

// composeDaemonGitConfig is the daemon's startup step for process-spawn law
// S6-D4: every daemon git command reads no system config and a global config
// composed from the operator's, keeping only allowlisted keys. A failure does
// not stop the daemon — daemon git then reads an EMPTY global config (fail
// closed), which the doctor's git_config_composition check reports as an
// ERROR, since forge fetch and push may need the operator's credential helper.
func (c *Container) composeDaemonGitConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := spawn.ComposeGitConfig(ctx, gitConfigDataDir())
	c.gitConfigComposition = toDoctorGitComposition(res, err)
	if err != nil {
		c.Logger.Error().Err(err).
			Msg("git config: could not compose the daemon's git config — daemon git reads an EMPTY global config (no credential helper, no ssh command) until this is fixed and the daemon restarted")
		return
	}
	ev := c.Logger.Info().Str("path", res.Path).Int("examined", res.Examined).Int("kept", len(res.Kept)).Int("dropped", len(res.Dropped))
	if len(res.Dropped) > 0 {
		keys := make([]string, 0, len(res.Dropped))
		for _, d := range res.Dropped {
			keys = append(keys, d.Key)
		}
		ev = ev.Strs("dropped_keys", keys)
	}
	ev.Msg("git config: composed the daemon's global git config from the operator's (allowlist, S6-D4)")
}

// toDoctorGitComposition converts spawn's result for the doctor, which does
// not import spawn (the per-symbol spawn law allows internal/api only
// GitHTTPBackend).
func toDoctorGitComposition(res spawn.GitConfigComposition, err error) *api.GitConfigComposition {
	out := &api.GitConfigComposition{Path: res.Path, Examined: res.Examined, Kept: len(res.Kept)}
	if err != nil {
		out.Err = err.Error()
	}
	for _, d := range res.Dropped {
		out.Dropped = append(out.Dropped, api.DroppedGitConfigKey{Key: d.Key, Reason: d.Reason})
	}
	return out
}
