package hostdoctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckPodmanConfig_NotInPath(t *testing.T) {
	// Override PATH so exec.LookPath("podman") fails. This is the
	// dominant production-environment failure mode (podman uninstalled
	// or removed from PATH), which the check exists to catch.
	t.Setenv("PATH", t.TempDir())
	h := &Checker{}
	got := h.checkPodmanConfig(t.Context())
	assert.Equal(t, "podman_config", got.Name)
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "podman not found in PATH")
}

func TestCheckAgentImages_NoConfigDir(t *testing.T) {
	h := &Checker{}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "agent_images", got.Name)
	assert.Equal(t, "SKIPPED", got.Status)
	assert.Contains(t, got.Message, "skipping image check")
}

func TestCheckAgentImages_LoadFailure(t *testing.T) {
	// configDir set but the swarms dir contains malformed SWARM.md
	// — LoadSwarms returns an error that surfaces as ERROR. (Stale
	// `.yaml` files are silently ignored post-YAML-removal, so the
	// failure trigger now lives on the MD frontmatter parse path.)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "broken.md"), []byte("not even a frontmatter marker"), 0o644))
	h := &Checker{configDir: dir}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "failed to load swarms")
}

func TestCheckAgentImages_PodmanMissing(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "")
	t.Setenv("PATH", t.TempDir()) // remove podman
	h := &Checker{configDir: dir}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "podman not found")
}

// New wires the daemon's revision from its report, never vornikctl's own build.
func TestNew_UsesTheDaemonsRevision(t *testing.T) {
	rev, ok := New("", "", "0123456789ab").resolveDaemonRevision()
	assert.True(t, ok)
	assert.Equal(t, "0123456789ab", rev)
	_, ok = New("", "", "").resolveDaemonRevision()
	assert.False(t, ok, "an unstated daemon revision is unknown, not this binary's")
}
