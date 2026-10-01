//go:build e2e_hermes

package hermes

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prepareHermesHome writes Hermes's config for the lane: the llama.cpp
// endpoint, the plugin copied from the working tree and enabled, and the
// memory provider under the name Hermes gives it (lane design, As built).
func prepareHermesHome(t *testing.T, s *stack) {
	t.Helper()
	home := filepath.Join(s.dir, "hermes-home")
	plugin := filepath.Join(home, "plugins", "vornik-companion")
	src := filepath.Join(repoRoot(t), "contrib", "hermes-companion")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "tests" || e.Name() == "__pycache__" {
			continue
		}
		run(t, "cp", "-r", filepath.Join(src, e.Name()), plugin+"/")
	}
	writeFile(t, filepath.Join(home, "config.yaml"), fmt.Sprintf(`model:
  default: %q
  provider: "custom"
  base_url: %q
  context_length: 65536
plugins:
  enabled: ["vornik-companion"]
  disabled: []
memory:
  provider: "vornik-companion"
# Hermes defers every plugin tool behind tool_search/tool_call by default;
# a small model does not search, so the lane offers the tools directly
# (lane design, As built).
tools:
  tool_search:
    enabled: "off"
# Only the plugin's toolset and memory (which carries the memory
# provider's tools): Hermes's default CLI toolsets make a 13,885-token
# system prompt and offer mail tools of their own, which a small model
# reaches for instead of Vornik (lane design, As built; §7 records what this
# leaves untested).
platform_toolsets:
  cli: ["vornik", "memory"]
`, s.hermesModelName, s.llamaURL))
	writeFile(t, filepath.Join(home, ".env"), "OPENAI_API_KEY=unused\n")
	s.hermesHome = home
}

// hermesRun is one `hermes -z` invocation.
type hermesRun struct {
	Prompt  string
	Reply   string // stdout: the final response
	Stderr  string
	Elapsed time.Duration
}

// runHermes runs one prompt in a fresh container against the lane's home.
// Afterwards the home is chowned back to the host user so the lane can
// scan it (Hermes drops to a remapped user inside the container).
func runHermes(t *testing.T, s *stack, prompt string) hermesRun {
	t.Helper()
	args := []string{"run", "--rm", "--network", "host",
		"-v", s.hermesHome + ":/opt/data:Z",
		"-e", "VORNIK_URL=" + s.hermesURL,
		"-e", "VORNIK_BROKER_TOKEN=" + s.brokerKey,
		"-e", "VORNIK_MEMORY_TOKEN=" + s.memoryKey,
		hermesImage, "hermes", "-z", prompt}
	cmd := exec.Command("podman", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := hermesRun{Prompt: prompt, Reply: strings.TrimSpace(stdout.String()), Stderr: stderr.String(), Elapsed: time.Since(start)}
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if err != nil {
		t.Logf("hermes -z exited: %v\n%s", err, tail(r.Stderr, 3000))
	}
	return r
}

// TestHermesBringup is a discovery run, not a scenario: one prompt through
// the whole stack, with Hermes's output and home listed for inspection.
func TestHermesBringup(t *testing.T) {
	ensureHermesImage(t)
	s := startStack(t)
	startHermesModel(t, s, Script{Marker: "Which Vornik workflows", Final: "Vornik can run mail-digest and mail-reply.",
		Steps: []ScriptStep{{ToolSuffix: "vornik_catalog", Args: map[string]any{}}}})
	prepareHermesHome(t, s)
	r := runHermes(t, s, "Which Vornik workflows can you run for me? Use your Vornik tools.")
	t.Logf("elapsed %s\nREPLY:\n%s\nSTDERR (tail):\n%s", r.Elapsed, r.Reply, tail(r.Stderr, 2500))
	if s.hermesModel != nil {
		t.Logf("tools Hermes offered its model: %v", s.hermesModel.ToolsSeen())
	}
	if keep := os.Getenv("VORNIK_E2E_KEEP"); keep != "" {
		_ = os.RemoveAll(keep)
		run(t, "cp", "-r", s.hermesHome, keep)
		t.Logf("kept Hermes home at %s", keep)
	}
	logs, _ := os.ReadFile(s.daemonLog)
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "/api/v1/capabilities") || strings.Contains(line, "/api/v1/mcp/companion") {
			t.Logf("daemon: %s", tail(line, 300))
		}
	}
}

// startHermesModel gives Hermes its model: llama.cpp by default, or in stub
// mode a scripted model following scripts (for harness debugging and for
// hosts that cannot run a model; never the release gate).
func startHermesModel(t *testing.T, s *stack, scripts ...Script) {
	t.Helper()
	if os.Getenv("VORNIK_E2E_HERMES_MODEL") == "stub" {
		s.hermesModel = &LLMStub{Final: "Done.", Scripts: scripts}
		s.hermesModelName = scriptedModel
		s.llamaURL = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, s.hermesModel))
		return
	}
	ensureHermesImage(t)
	m := pickModel(t)
	s.hermesModelName = m.Name
	s.hermesRec = NewToolRecorder(strings.TrimSuffix(startLlama(t, m, ensureModel(t, m)), "/v1"))
	s.llamaURL = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, s.hermesRec))
}
