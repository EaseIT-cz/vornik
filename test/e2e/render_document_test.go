//go:build e2e
// +build e2e

package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/sandboxtool"
)

// render_document end to end (process-spawn law S4,
// https://docs.vornik.io). The chat tool
// used to run pandoc on the daemon host; it now renders only inside the agent
// image, with no network. These tests drive the real tool path — the
// dispatcher's render_document through Agent.ExecuteTool, the production
// podman runner, the real agent image built from this tree — and check real
// output files, so they prove rendering still WORKS, not only that host pandoc
// is gone.

// recordedSender keeps every delivered file.
type recordedSender struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *recordedSender) SendArtifactFile(_ context.Context, name string, r io.Reader, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[name] = b
	return nil
}

// recordingRunner wraps the production command runner and keeps the argv of
// each container it started (`podman run`; the runner's inspect and rm calls
// are passed through unrecorded).
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "run" {
		r.mu.Lock()
		r.calls = append(r.calls, append([]string{name}, args...))
		r.mu.Unlock()
	}
	return sandboxtool.ExecCommand(ctx, stdin, name, args...)
}

// hostPandocTrap puts a fake `pandoc` first on PATH that leaves a marker if
// anything runs pandoc on the host, so "no host fallback" is observed rather
// than assumed.
func hostPandocTrap(t *testing.T) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "host-pandoc-ran")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pandoc"), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// renderAgent is a dispatcher whose render_document runs through a real
// sandboxtool runner (S5a) with its scratch under scratch.
func renderAgent(t *testing.T, image, scratch string, rec *recordingRunner, mod func(*sandboxtool.Config)) *dispatcher.Agent {
	t.Helper()
	cfg := sandboxtool.Config{Image: image, ScratchRoot: scratch}
	if mod != nil {
		mod(&cfg)
	}
	r, err := sandboxtool.New(cfg, rec.run)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher.NewAgent(nil, nil, nil, nil, nil, dispatcher.WithSandboxRunner(r))
}

// scratchIsEmpty: the runner removes every run's scratch, whatever the outcome.
func scratchIsEmpty(t *testing.T, scratch string) {
	t.Helper()
	entries, err := os.ReadDir(scratch)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch left behind: %d entries under %s", len(entries), scratch)
	}
}

func TestRenderDocumentE2E_RendersRealFilesInTheSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("render_document e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	marker := hostPandocTrap(t)

	rec := &recordingRunner{}
	fs := &recordedSender{}
	// Shell metacharacters in the content and spaces in the name: neither may
	// reach argv, and both must render intact.
	pwned := filepath.Join(t.TempDir(), "pwned")
	content := "# Quarterly report\n\nTotals: $(touch " + pwned + ") `touch " + pwned + "` ; | && > <\n\n- one\n- two\n"
	args := `{"content":` + jsonString(content) + `,"name":"quarterly report","formats":["md","html","pdf","docx"]}`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	scratch := t.TempDir()
	res := renderAgent(t, image, scratch, rec, nil).ExecuteTool(ctx, "render_document", args, fs)
	if !strings.Contains(res.Content, "Delivered: quarterly report.md, quarterly report.html, quarterly report.pdf, quarterly report.docx") {
		t.Fatalf("render_document: %s", res.Content)
	}

	pdf := fs.files["quarterly report.pdf"]
	if len(pdf) < 100 || !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Errorf("the PDF is not a PDF (%d bytes): %q", len(pdf), firstBytes(pdf))
	}
	docx := fs.files["quarterly report.docx"]
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Errorf("the DOCX is not a zip: %v", err)
	} else if !zipHas(zr, "word/document.xml") {
		t.Error("the DOCX has no word/document.xml")
	}
	html := string(fs.files["quarterly report.html"])
	if !strings.Contains(html, "<html") || !strings.Contains(html, "Quarterly report") || !strings.Contains(html, "$(touch") {
		t.Errorf("the HTML lacks the document, or mangled its metacharacters: %.300s", html)
	}

	if len(rec.calls) != 3 {
		t.Fatalf("want one sandbox run per rendered format, got %d", len(rec.calls))
	}
	for _, call := range rec.calls {
		joined := strings.Join(call, " ")
		if call[0] != "podman" {
			t.Errorf("only podman may run: %s", joined)
		}
		for _, want := range []string{
			"--network=none", "--pull=never", image,
			// S5a: the render feature's limits and the fixed hardening, and
			// the real image still renders under all of them.
			"--memory=1073741824", "--memory-swap=1073741824", "--cpus=1", "--pids-limit=64",
			"--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only",
			"--tmpfs /tmp:rw,size=256m",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv lacks %q: %s", want, joined)
			}
		}
		for _, userText := range []string{"quarterly", "touch", "$("} {
			if strings.Contains(joined, userText) {
				t.Errorf("user text %q reached argv: %s", userText, joined)
			}
		}
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("a metacharacter in the content was executed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("host pandoc ran")
	}
	scratchIsEmpty(t, scratch)
}

// S5a: a render that outlives its feature timeout is killed and reported as
// "timed out", and its scratch is removed.
func TestRenderDocumentE2E_TimeoutIsReported(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("render_document e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	rec := &recordingRunner{}
	scratch := t.TempDir()
	agent := renderAgent(t, image, scratch, rec, func(c *sandboxtool.Config) {
		c.Timeouts = map[sandboxtool.Feature]time.Duration{sandboxtool.FeatureRender: 50 * time.Millisecond}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := agent.ExecuteTool(ctx, "render_document", `{"content":"# Hi","name":"cv","formats":["pdf"]}`, &recordedSender{})
	if !strings.Contains(res.Content, "timed out") {
		t.Fatalf("want timed out, got: %s", res.Content)
	}
	scratchIsEmpty(t, scratch)
	// The killed run's named container is removed, not left behind.
	for _, call := range rec.calls {
		for i, a := range call {
			if a == "--name" && i+1 < len(call) {
				if exec.Command("podman", "container", "exists", call[i+1]).Run() == nil {
					t.Errorf("container %s survived its timeout", call[i+1])
				}
			}
		}
	}
}

// S5a: an oversize document is refused before any container starts.
func TestRenderDocumentE2E_OversizeInputIsRefusedBeforeAnyContainer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("render_document e2e requires Linux + podman")
	}
	requirePodman(t)
	rec := &recordingRunner{}
	agent := renderAgent(t, "localhost/vornik-agent:e2e-unused", t.TempDir(), rec, func(c *sandboxtool.Config) {
		c.MaxInputBytes = 1024
	})
	content := strings.Repeat("oversize ", 1024)
	res := agent.ExecuteTool(context.Background(), "render_document",
		`{"content":`+jsonString(content)+`,"name":"cv","formats":["html"]}`, &recordedSender{})
	if !strings.Contains(res.Content, "input too large") {
		t.Fatalf("want input too large, got: %s", res.Content)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("no container may start for an oversize input: %v", rec.calls)
	}
}

func TestRenderDocumentE2E_ImageAbsentIsNotAvailableWithNoHostFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("render_document e2e requires Linux + podman")
	}
	requirePodman(t)
	marker := hostPandocTrap(t)

	rec := &recordingRunner{}
	fs := &recordedSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := renderAgent(t, "localhost/vornik-agent:e2e-does-not-exist", t.TempDir(), rec, nil).ExecuteTool(ctx, "render_document",
		`{"content":"# Hi","name":"cv","formats":["html","pdf"]}`, fs)
	if !strings.Contains(res.Content, "not available in the agent image") {
		t.Fatalf("an absent image must report not available, got: %s", res.Content)
	}
	if len(fs.files) != 0 {
		t.Errorf("nothing may be delivered, got %v", keys(fs.files))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("host pandoc ran as a fallback")
	}
}

func zipHas(zr *zip.Reader, name string) bool {
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

func firstBytes(b []byte) []byte {
	if len(b) > 16 {
		return b[:16]
	}
	return b
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
