package dispatcher

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/sandboxtool"
)

// Process-spawn law, S4 (https://docs.vornik.io):
// render_document is a chat tool, so any chat channel can trigger it. It used to
// run pandoc on the daemon HOST, falling back to podman only when pandoc was
// missing. It now renders only inside the pinned agent image, with no network
// and no user text in argv — and, since S5a, through the sandboxtool runner,
// which carries the limits, hardening and timeout.

const testRenderImage = "ghcr.io/easeit-cz/vornik-agent:latest"

type renderCall struct {
	name string
	args []string
}

// fakeSandbox records every podman run and, like pandoc in the container,
// writes the requested /out file into the host directory mounted there. The
// runner's own inspect and rm calls are answered and not recorded.
type fakeSandbox struct {
	calls []renderCall
	meta  string
	fail  error
	out   string
	block bool // wait for the run's context to end, like a hung pandoc
}

func (f *fakeSandbox) run(ctx context.Context, _ io.Reader, name string, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "run" {
		if len(args) > 0 && args[0] == "inspect" {
			return []byte("false\n"), nil
		}
		return nil, nil
	}
	f.calls = append(f.calls, renderCall{name, append([]string(nil), args...)})
	if f.block {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	if f.fail != nil {
		return []byte(f.out), f.fail
	}
	var inDir, outDir, outFile string
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			spec := args[i+1]
			if host, ok := strings.CutSuffix(spec, ":/in:ro,Z"); ok {
				inDir = host
			}
			if host, ok := strings.CutSuffix(spec, ":/out:Z"); ok {
				outDir = host
			}
		}
		if a == "-o" && i+1 < len(args) {
			outFile = strings.TrimPrefix(args[i+1], "/out/")
		}
	}
	if b, err := os.ReadFile(filepath.Join(inDir, "meta.yaml")); err == nil {
		f.meta = string(b)
	}
	return nil, os.WriteFile(filepath.Join(outDir, outFile), []byte("rendered"), 0o600)
}

func newTestSandbox(t *testing.T, cfg sandboxtool.Config, run sandboxtool.CommandRunner) *sandboxtool.Runner {
	t.Helper()
	if cfg.ScratchRoot == "" {
		cfg.ScratchRoot = t.TempDir()
	}
	r, err := sandboxtool.New(cfg, run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sandboxExecutor(t *testing.T, sb *fakeSandbox) *ToolExecutor {
	t.Helper()
	return &ToolExecutor{sandboxRunner: newTestSandbox(t, sandboxtool.Config{Image: testRenderImage}, sb.run)}
}

func TestRenderDocument_RendersOnlyInTheSandbox(t *testing.T) {
	sb := &fakeSandbox{}
	te := sandboxExecutor(t, sb)
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(),
		`{"content":"# USERTEXT --flag","name":"curriculum","formats":["html","pdf"]}`, fs)
	if !strings.Contains(res.Content, "Delivered: curriculum.html, curriculum.pdf") {
		t.Fatalf("got %q", res.Content)
	}
	if len(sb.calls) != 2 {
		t.Fatalf("want one sandbox run per format, got %d", len(sb.calls))
	}
	for _, c := range sb.calls {
		if c.name != "podman" {
			t.Fatalf("only podman may run; got %q", c.name)
		}
		joined := strings.Join(c.args, " ")
		for _, want := range []string{
			"run", "--network=none", "--pull=never", "--entrypoint pandoc", testRenderImage,
			// S5a: the render feature's limits and the fixed hardening.
			"--memory=1073741824", "--memory-swap=1073741824", "--cpus=1", "--pids-limit=64",
			"--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv lacks %q: %s", want, joined)
			}
		}
		for _, userText := range []string{"USERTEXT", "--flag", "curriculum"} {
			if strings.Contains(joined, userText) {
				t.Errorf("user text %q reached argv: %s", userText, joined)
			}
		}
	}
	if !strings.Contains(sb.meta, "curriculum") {
		t.Errorf("the title travels in the mounted metadata file, got %q", sb.meta)
	}
}

// docx is pandoc-native: no extra engine, and a fixed output name.
func TestRenderDocument_Docx(t *testing.T) {
	sb := &fakeSandbox{}
	te := sandboxExecutor(t, sb)
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["docx"]}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "Delivered: cv.docx") {
		t.Fatalf("got %q", res.Content)
	}
	joined := strings.Join(sb.calls[0].args, " ")
	if !strings.Contains(joined, "-o /out/output.docx") || strings.Contains(joined, "--pdf-engine") {
		t.Fatalf("docx argv: %s", joined)
	}
}

func TestRenderDocument_NoAgentImageMeansNotAvailable(t *testing.T) {
	sb := &fakeSandbox{}
	for name, te := range map[string]*ToolExecutor{
		"no image":  {sandboxRunner: newTestSandbox(t, sandboxtool.Config{}, sb.run)},
		"no runner": {},
	} {
		res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["html"]}`, &stubSenderRecording{})
		if !strings.Contains(res.Content, "rendering not available in the agent image") {
			t.Fatalf("%s: got %q", name, res.Content)
		}
	}
	if len(sb.calls) != 0 {
		t.Fatalf("nothing may run without the pinned image: %+v", sb.calls)
	}
}

func TestRenderDocument_MissingToolInTheImageIsNotAvailable(t *testing.T) {
	sb := &fakeSandbox{fail: errors.New("exit status 127"), out: `Error: crun: executable file "pandoc" not found in $PATH: No such file or directory`}
	te := sandboxExecutor(t, sb)
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["pdf"]}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "rendering not available in the agent image") {
		t.Fatalf("got %q", res.Content)
	}
	if len(sb.calls) != 1 {
		t.Fatalf("no host fallback may follow a failed sandbox run: %+v", sb.calls)
	}
}

// S4 review F6: pandoc's own failure that happens to exit 127 (or 125) must be
// reported as the failure it is, not masked as "not available".
func TestRenderDocument_PandocFailureIsNotMaskedAsNotAvailable(t *testing.T) {
	sb := &fakeSandbox{fail: errors.New("exit status 127"), out: "Error parsing YAML metadata at \"/in/meta.yaml\""}
	te := sandboxExecutor(t, sb)
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["pdf"]}`, &stubSenderRecording{})
	if strings.Contains(res.Content, "not available") || !strings.Contains(res.Content, "Error parsing YAML metadata") {
		t.Fatalf("got %q", res.Content)
	}
}

// S5a: a hung pandoc is stopped at the render feature's timeout and reported.
func TestRenderDocument_TimeoutIsReported(t *testing.T) {
	sb := &fakeSandbox{block: true}
	te := &ToolExecutor{sandboxRunner: newTestSandbox(t, sandboxtool.Config{
		Image:    testRenderImage,
		Timeouts: map[sandboxtool.Feature]time.Duration{sandboxtool.FeatureRender: 20 * time.Millisecond},
	}, sb.run)}
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["html"]}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "timed out") {
		t.Fatalf("got %q", res.Content)
	}
}

// S5a: an oversize document is refused before any container starts.
func TestRenderDocument_OversizeInputIsRefusedBeforeAnyContainer(t *testing.T) {
	sb := &fakeSandbox{}
	te := &ToolExecutor{sandboxRunner: newTestSandbox(t, sandboxtool.Config{Image: testRenderImage, MaxInputBytes: 64}, sb.run)}
	res := te.renderDocument(context.Background(), `{"content":"`+strings.Repeat("x", 200)+`","name":"cv","formats":["html"]}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "input too large") {
		t.Fatalf("got %q", res.Content)
	}
	if len(sb.calls) != 0 {
		t.Fatalf("no container may start for an oversize input: %+v", sb.calls)
	}
}

// With the real runner and no podman on PATH, nothing falls back to a host
// pandoc either.
func TestRenderDocument_NoPodmanIsNotAvailable(t *testing.T) {
	t.Setenv("PATH", "")
	te := &ToolExecutor{sandboxRunner: newTestSandbox(t, sandboxtool.Config{Image: testRenderImage}, nil)}
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"cv","formats":["html"]}`, &stubSenderRecording{})
	if !strings.Contains(res.Content, "not available") {
		t.Fatalf("got %q", res.Content)
	}
}

// S4 review F5: the output file name cannot leave the render directory, even
// if a name ever got past safepath.CleanFileName.
func TestRenderPath_CannotTraverse(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "a/b", "..", ".", `a\b`, ""} {
		if p, err := renderPath(dir, name, "pdf"); err == nil {
			t.Errorf("renderPath(%q) = %q, want refused", name, p)
		}
	}
	p, err := renderPath(dir, "cv", "pdf")
	if err != nil || filepath.Base(p) != "cv.pdf" {
		t.Fatalf("renderPath(cv) = %q, %v", p, err)
	}
}

func TestRenderDocument_TraversingNameIsRefused(t *testing.T) {
	sb := &fakeSandbox{}
	te := sandboxExecutor(t, sb)
	fs := &stubSenderRecording{}
	res := te.renderDocument(context.Background(), `{"content":"# Hi","name":"../../etc/cv","formats":["md","html"]}`, fs)
	if !strings.Contains(res.Content, "invalid name") || len(fs.paths) != 0 || len(sb.calls) != 0 {
		t.Fatalf("got %q, sent %v, runs %d", res.Content, fs.paths, len(sb.calls))
	}
}
