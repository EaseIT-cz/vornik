package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #71: a brokered role must render a staged Czech markdown document
// without shell access. This first exercises the actual helper dispatch seam.
func TestDocumentRenderStagedMarkdown(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "artifacts/in/report.md"), "# Praha\n\npříliš žluťoučký kůň — čřžšýáíéúůě")
	if err := os.Chmod(filepath.Join(ws, "artifacts/in/report.md"), 0444); err != nil {
		t.Fatal(err)
	}
	got := Dispatch(Env{Workspace: ws}, "document_render", json.RawMessage(`{"path":"artifacts/in/report.md","format":"html"}`))
	// The hermetic host lane can lack pandoc; it must reach the renderer's
	// explicit failure instead of refusing this undeclared/unimplemented tool.
	if strings.Contains(got, "does not run in the helper") {
		t.Fatalf("document renderer unavailable: %s", got)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Path  string `json:"path"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		t.Fatalf("structured render result: %s", got)
	}
	if !result.OK && result.Stage != "pandoc" {
		t.Fatalf("unexpected render failure: %s", got)
	}
	if result.OK && result.Path != "artifacts/out/report.html" {
		t.Fatalf("wrong artifact path: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(ws, "artifacts/in/report.md"))
	if err != nil || !strings.Contains(string(data), "čřžšýáíéúůě") {
		t.Fatalf("staged source changed: %q %v", data, err)
	}
}

func stubDocumentRenderer(t *testing.T, mode string) documentRenderer {
	t.Helper()
	return documentRenderer{timeout: time.Second, command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		switch name {
		case "/usr/bin/pandoc":
			if len(args) != 4 || strings.Join(args[:3], " ") != "--from=gfm --to=html5 --standalone" || !strings.HasPrefix(args[3], "--lua-filter=") {
				t.Fatalf("unsafe pandoc argv: %v", args)
			}
			filter, err := os.ReadFile(strings.TrimPrefix(args[3], "--lua-filter="))
			if err != nil || string(filter) != documentHTMLFilter {
				t.Fatalf("untrusted/missing filter: %v", err)
			}
		case "/usr/bin/python3":
			if len(args) != 3 || args[0] != "-I" || args[1] != "-c" || args[2] != documentPDFScript {
				t.Fatalf("unsafe Python argv: %v", args)
			}
		default:
			t.Fatalf("unexpected renderer command: %s", name)
		}
		var script string
		switch mode {
		case "fail":
			script = "printf 'missing converter' >&2; exit 1"
		case "timeout":
			script = "exec /bin/sleep 2"
		case "overflow":
			script = "exec /usr/bin/head -c 16777217 /dev/zero"
		case "empty":
			script = "exit 0"
		default:
			script = `if [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}${PYTHONPATH:-}${VORNIK_COMPANION_TOKEN:-}" ]; then echo credential-leak >&2; exit 1; fi; /bin/cat`
			if name == "/usr/bin/python3" {
				script += "; printf '%%PDF-fake'"
				if mode != "invalid pdf" {
					script = "/bin/cat >/dev/null; printf '%%PDF-fake'"
				}
			}
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}}
}

func TestDocumentRenderSubprocessAndFailures(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-secret")
	t.Setenv("GITHUB_TOKEN", "test-secret")
	t.Setenv("PYTHONPATH", "/untrusted")
	t.Setenv("VORNIK_COMPANION_TOKEN", "test-secret")
	for _, tc := range []struct{ mode, format, stage string }{
		{"ok", "html", ""}, {"ok", "pdf", ""}, {"fail", "html", "pandoc"},
		{"overflow", "html", "pandoc"}, {"empty", "html", "render"}, {"invalid pdf", "pdf", "weasyprint"},
		{"timeout", "html", "pandoc"},
	} {
		t.Run(tc.mode+tc.format, func(t *testing.T) {
			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, "report.md"), "# Praha\npříliš žluťoučký kůň")
			r := stubDocumentRenderer(t, tc.mode)
			if tc.mode == "timeout" {
				r.timeout = 10 * time.Millisecond
			}
			raw, _ := json.Marshal(documentRenderArgs{Path: "report.md", Format: tc.format})
			got := renderDocumentWith(Env{Workspace: ws}, raw, r)
			var out documentRenderResult
			if err := json.Unmarshal([]byte(got), &out); err != nil {
				t.Fatal(err)
			}
			if len(got) >= 4096 {
				t.Fatalf("audit result too long: %d", len(got))
			}
			if tc.stage != "" {
				if out.OK || out.Stage != tc.stage || out.Error == "" {
					t.Fatalf("bad failure: %s", got)
				}
				if _, err := os.Stat(filepath.Join(ws, "artifacts/out/report."+tc.format)); !os.IsNotExist(err) {
					t.Fatalf("failed conversion wrote output: %v", err)
				}
				return
			}
			if !out.OK || !strings.HasPrefix(got, `{"path":`) {
				t.Fatalf("bad success: %s", got)
			}
			data, err := os.ReadFile(filepath.Join(ws, out.Path))
			if err != nil || len(data) != out.Bytes {
				t.Fatalf("wrong output: %v %s", err, got)
			}
			if tc.format == "html" && !strings.Contains(string(data), "příliš žluťoučký kůň") {
				t.Fatal("Unicode lost")
			}
			// No retries may overwrite a prior artifact.
			if again := renderDocumentWith(Env{Workspace: ws}, raw, r); !strings.Contains(again, `"stage":"write"`) {
				t.Fatalf("overwrote existing output: %s", again)
			}
		})
	}
}

func TestDocumentRenderPathsAndInput(t *testing.T) {
	for _, tc := range []struct{ name, args, stage string }{
		{"path required", `{"format":"html"}`, "arguments"},
		{"format required", `{"path":"report.md"}`, "arguments"},
		{"unknown flags", `{"path":"report.md","format":"html","flags":"--filter=evil"}`, "arguments"},
		{"invalid json", `{`, "arguments"},
		{"trailing json", `{"path":"report.md","format":"html"}{}`, "arguments"},
		{"html source", `{"path":"report.html","format":"pdf"}`, "resolve"},
		{"traversal", `{"path":"../outside.md","format":"html"}`, "resolve"},
		{"outside output", `{"path":"report.md","format":"html","out_path":"../out.html"}`, "resolve"},
		{"input output", `{"path":"report.md","format":"html","out_path":"artifacts/in/out.html"}`, "resolve"},
		{"absolute output", `{"path":"report.md","format":"html","out_path":"/tmp/out.html"}`, "resolve"},
		{"wrong suffix", `{"path":"report.md","format":"html","out_path":"artifacts/out/out.pdf"}`, "resolve"},
		{"missing", `{"path":"missing.md","format":"html"}`, "read"},
		{"directory", `{"path":"directory.md","format":"html"}`, "read"},
		{"invalid UTF8", `{"path":"invalid.md","format":"html"}`, "read"},
		{"source cap", `{"path":"big.md","format":"html"}`, "read"},
		{"spill alias", `{"path":"alias.md","format":"html"}`, "resolve"},
		{"symlink source", `{"path":"escape.md","format":"html"}`, "resolve"},
		{"symlink output", `{"path":"report.md","format":"html","out_path":"artifacts/out/link.html"}`, "write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, "report.md"), "original")
			mustWrite(t, filepath.Join(ws, "invalid.md"), "\xff")
			mustWrite(t, filepath.Join(ws, "big.md"), strings.Repeat("x", documentSourceCap+1))
			mustWrite(t, filepath.Join(ws, ".tool_results/spill.md"), "secret spill")
			outside := filepath.Join(t.TempDir(), "outside.md")
			mustWrite(t, outside, "outside canary")
			for link, target := range map[string]string{"escape.md": outside, "alias.md": filepath.Join(ws, ".tool_results/spill.md"), "artifacts/out/link.html": outside} {
				p := filepath.Join(ws, link)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(ws, "directory.md"), 0755); err != nil {
				t.Fatal(err)
			}
			got := renderDocumentWith(Env{Workspace: ws}, json.RawMessage(tc.args), stubDocumentRenderer(t, "ok"))
			if !strings.Contains(got, `"stage":"`+tc.stage+`"`) || strings.Contains(got, `"ok":true`) {
				t.Fatalf("wrong refusal: %s", got)
			}
			b, err := os.ReadFile(outside)
			if err != nil || string(b) != "outside canary" {
				t.Fatalf("outside file changed: %q %v", b, err)
			}
		})
	}
}

func TestDocumentRenderBoundedResult(t *testing.T) {
	got := documentFailure(strings.Repeat("bad", 4096), "arguments", fmt.Errorf("%s", strings.Repeat("\x01", 4096)))
	if len(got) >= 4096 || !json.Valid([]byte(got)) {
		t.Fatalf("unbounded/invalid audit result: %d", len(got))
	}
}

func TestDocumentRenderRefusesOutputDirectoryAliases(t *testing.T) {
	for _, alias := range []string{"artifacts", "artifacts/out", "artifacts/out/nested"} {
		t.Run(alias, func(t *testing.T) {
			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, "report.md"), "# source")
			mustWrite(t, filepath.Join(ws, "input/canary.md"), "original")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, alias)), 0755); err != nil {
				t.Fatal(err)
			}
			target, err := filepath.Rel(filepath.Dir(filepath.Join(ws, alias)), filepath.Join(ws, "input"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(ws, alias)); err != nil {
				t.Fatal(err)
			}
			dst := alias + "/report.html"
			if alias == "artifacts" {
				dst = "artifacts/out/report.html"
			}
			raw, _ := json.Marshal(documentRenderArgs{Path: "report.md", Format: "html", OutPath: dst})
			got := renderDocumentWith(Env{Workspace: ws}, raw, stubDocumentRenderer(t, "ok"))
			if !strings.Contains(got, `"stage":"write"`) {
				t.Fatalf("output alias allowed: %s", got)
			}
			if _, err := os.Stat(filepath.Join(ws, "input/report.html")); !os.IsNotExist(err) {
				t.Fatalf("wrote through alias: %v", err)
			}
		})
	}
}
