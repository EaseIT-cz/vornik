package hostdoctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageIsUnqualified is the pure classifier behind the agent_images
// severity split. Tested directly so the regression guarantee does not
// depend on podman being installed in CI.
func TestImageIsUnqualified(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		// Bare names — the shape that broke every job on 2026-06-27.
		{"vornik-agent:latest", true},
		{"swarmd-agent:latest", true},
		{"ubuntu", true},
		// A single-slash docker-library shorthand is still a short name:
		// podman must consult unqualified-search-registries to resolve it.
		{"library/ubuntu", true},
		// Qualified references — a registry component is present, so a
		// missing copy can still be pulled non-interactively.
		{"localhost/vornik-agent:latest", false},
		{"docker.io/library/golang:1.25", false},
		{"quay.io/podman/hello", false},
		{"myregistry:5000/team/app:v2", false},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, imageIsUnqualified(c.ref), "imageIsUnqualified(%q)", c.ref)
	}
}

// writeSwarmWithImage drops a minimal single-role swarm + project so
// checkAgentImages has something to load, with the role image under test.
func writeSwarmWithImage(t *testing.T, root, image string) {
	t.Helper()
	writeAll(t, root, map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "` + image + `" }
---
`,
		"projects/p.yaml": "projectId: \"p\"\nswarmId: \"s\"\n",
	})
}

// TestCheckAgentImages_MissingShortNameIsError is the regression test for
// the 2026-06-27 incident: the swarmd→vornik rename left deployed swarm
// configs pointing at the unbuilt short name `swarmd-agent:latest`. With
// `short-name-mode = enforced` (the host default) podman cannot resolve a
// short name without a TTY, so every job died at container start. The
// agent_images doctor check had reported missing images as a benign
// WARNING ("will be pulled on first use") — true only for qualified
// references. A missing *short-name* image is unpullable and must be an
// ERROR so an operator catches it before the next job fails.
func TestCheckAgentImages_MissingShortNameIsError(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not installed; ERROR-path needs a real `podman image exists` miss")
	}
	dir := t.TempDir()
	// A name that cannot exist locally — exercises the missing branch.
	writeSwarmWithImage(t, dir, "vornik-doctor-absent-shortname-xyz:latest")
	h := &Checker{configDir: dir}

	got := h.checkAgentImages(t.Context())

	assert.Equal(t, "agent_images", got.Name)
	assert.Equal(t, "ERROR", got.Status, "missing short-name image must be ERROR, not WARNING")
	assert.Contains(t, got.Items, "vornik-doctor-absent-shortname-xyz:latest")
}

// A missing *qualified* image can still be pulled, so it stays a WARNING
// rather than blocking the operator with a false ERROR.
func TestCheckAgentImages_MissingQualifiedIsWarning(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not installed")
	}
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "docker.io/library/vornik-doctor-absent-xyz:latest")
	h := &Checker{configDir: dir}

	got := h.checkAgentImages(t.Context())

	assert.Equal(t, "WARNING", got.Status, "missing qualified image is pullable → WARNING")
}

// audit 2026-10-12 (w2-6), GitHub #309: a missing localhost/ image was rated
// WARNING ("will be pulled on first use"), but no registry serves localhost/,
// so podman cannot pull it and every container start that needs it fails.
// That is how a missing localhost/vornik-agent:bench failed a whole bench
// chunk at container start. It must be an ERROR naming how to build/tag it.
func TestCheckAgentImages_MissingLocalhostIsError(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not installed")
	}
	dir := t.TempDir()
	const ref = "localhost/vornik-doctor-absent-xyz:bench"
	writeSwarmWithImage(t, dir, ref)
	h := &Checker{configDir: dir}

	got := h.checkAgentImages(t.Context())

	assert.Equal(t, "ERROR", got.Status, "a missing localhost/ image cannot be pulled → ERROR")
	assert.Contains(t, got.Items, ref)
	assert.Contains(t, got.Message, "podman tag", "the remedy names the tag command")
}

// The severity table of image-freshness design §18, without podman.
func TestClassifyMissingAgentImages(t *testing.T) {
	pullable := classifyMissingAgentImages([]string{"quay.io/x/y:1", "localhost:5000/team/app:v2"})
	assert.Equal(t, "WARNING", pullable.Status, "registry-qualified (incl. localhost:5000) misses are pullable")
	assert.Equal(t, []string{"localhost:5000/team/app:v2", "quay.io/x/y:1"}, pullable.Items)

	local := classifyMissingAgentImages([]string{"quay.io/x/y:1", "localhost/vornik-agent:bench"})
	assert.Equal(t, "ERROR", local.Status)
	assert.Equal(t, []string{"localhost/vornik-agent:bench"}, local.Items, "only the unpullable refs are listed")
	assert.Contains(t, local.Message, "make build-agent && podman tag ghcr.io/easeit-cz/vornik-agent:latest localhost/vornik-agent:bench")

	other := classifyMissingAgentImages([]string{"localhost/vornik-scraper:latest"})
	assert.Equal(t, "ERROR", other.Status)
	assert.Contains(t, other.Message, "make install-images")
	assert.NotContains(t, other.Message, "make build-agent")

	both := classifyMissingAgentImages([]string{"vornik-agent:latest", "localhost/x:1"})
	assert.Equal(t, "ERROR", both.Status)
	assert.Equal(t, []string{"localhost/x:1", "vornik-agent:latest"}, both.Items)
	assert.Contains(t, both.Message, "short name")
	assert.Contains(t, both.Message, "localhost/")

	// Digests (review-20261012-09f4): a digest does not make localhost/
	// pullable; a registry-qualified digest stays pullable. A digest-pinned
	// agent ref cannot be the target of `podman tag`, so it gets the generic
	// remedy.
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ld := classifyMissingAgentImages([]string{"localhost/vornik-agent" + digest})
	assert.Equal(t, "ERROR", ld.Status)
	assert.NotContains(t, ld.Message, "make build-agent")
	assert.Equal(t, "WARNING", classifyMissingAgentImages([]string{"quay.io/x/y" + digest}).Status)

	assert.True(t, imageIsLocalOnly("localhost/a:b"))
	assert.False(t, imageIsLocalOnly("localhost:5000/a:b"))
	assert.False(t, imageIsLocalOnly("docker.io/localhost/a"))
}

// Guard the test fixtures actually land on disk (catches a writeAll regression).
func TestWriteSwarmWithImage_Lands(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	_, err := os.Stat(filepath.Join(dir, "swarms", "s.md"))
	require.NoError(t, err)
}
