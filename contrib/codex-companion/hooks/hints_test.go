package hooks_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHub #634 slice 4 (LLD 2026-08-20 companion hardening §13.12): the Codex
// bundle's hint hooks. hints_test.sh pins the tool-event modes with
// Codex-shaped payloads; the cases below pin the copy rule and the
// SessionStart network turn.

func codexHooksDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCodexHintsHook_634(t *testing.T) {
	cmd := exec.Command("bash", filepath.Join(codexHooksDir(t), "hints_test.sh"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hints_test.sh failed: %v\n---\n%s", err, out.String())
	}
}

// One script, two bundles (§13.12 X4): the Codex copy is byte-equal to the
// Claude Code one.
func TestCodexHints_ScriptEqualsClaudeCodeCopy_634(t *testing.T) {
	here := codexHooksDir(t)
	codex, err := os.ReadFile(filepath.Join(here, "hints.sh"))
	if err != nil {
		t.Fatal(err)
	}
	claude, err := os.ReadFile(filepath.Join(here, "..", "..", "claude-code-companion", "hooks", "hints.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(codex, claude) {
		t.Fatal("contrib/codex-companion/hooks/hints.sh differs from the Claude Code copy; copy it over")
	}
}

// No active hint ships without a follow Codex can credit (§13.12 X3): a
// recall or a delegate target. A skill target never credits in Codex.
func TestCodexHints_EveryActiveHintCreditable_634(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(codexHooksDir(t), "hints.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Hints []struct {
			ID     string   `json:"id"`
			Active bool     `json:"active"`
			Follow []string `json:"follow"`
		} `json:"hints"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, h := range cat.Hints {
		if !h.Active {
			continue
		}
		active++
		ok := false
		for _, f := range h.Follow {
			if f == "recall" || strings.HasPrefix(f, "delegate:") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("active hint %s has no follow Codex can credit: %v", h.ID, h.Follow)
		}
	}
	if active == 0 {
		t.Fatal("examined no active hint")
	}
}

// slowDaemon answers the catalogue only after release is closed, so the test
// can see the SessionStart hook exit while its detached turn still runs.
type slowDaemon struct {
	release chan struct{}
	mu      sync.Mutex
	got     []string
}

func (d *slowDaemon) handler(w http.ResponseWriter, r *http.Request) {
	<-d.release
	d.mu.Lock()
	d.got = append(d.got, r.URL.Path+" "+r.Header.Get("User-Agent"))
	d.mu.Unlock()
	if r.URL.Path == "/api/v1/companion/hints" {
		_, _ = io.WriteString(w, `{"project":"p","enabled":true,"hints":[],"muted":[]}`)
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func (d *slowDaemon) paths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

// session-start codex reads the session id from stdin, prints nothing,
// returns while the detached turn still runs, and the turn then fetches the
// catalogue with the Codex User-Agent (§13.12 X2, X3, X5).
func TestCodexHintsSessionStart_634(t *testing.T) {
	d := &slowDaemon{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(codexHooksDir(t), "hints.sh"), "session-start", "codex")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"PLUGIN_DATA=" + data,
		"CLAUDE_PLUGIN_DATA=" + data,
		"VORNIK_URL=" + srv.URL,
		"VORNIK_COMPANION_TOKEN=test-token",
	}
	cmd.Stdin = strings.NewReader(`{"session_id":"cx1","cwd":"/w","hook_event_name":"SessionStart","source":"startup"}`)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		close(d.release)
		t.Fatalf("session-start exited %v: %s", err, out.String())
	}
	if out.Len() != 0 {
		close(d.release)
		t.Fatalf("session-start printed: %q", out.String())
	}
	if len(d.paths()) != 0 {
		close(d.release)
		t.Fatal("the hook waited for the daemon: it must return while the detached turn runs")
	}
	close(d.release)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(data, "hints", "network.log")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := d.paths()
	if len(got) == 0 || !strings.HasPrefix(got[0], "/api/v1/companion/hints vornik-companion/") || !strings.HasSuffix(got[0], "(codex)") {
		t.Fatalf("the detached turn did not fetch with the Codex User-Agent: %v (hook took %s)", got, time.Since(start))
	}
}
