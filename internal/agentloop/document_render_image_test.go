package agentloop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #71: run the actual helper and converters in the pinned agent image.
// This lane is opt-in because it requires Podman and that image locally:
// VORNIK_IMAGE_SMOKE=1 go test ./internal/agentloop -run TestDocumentRenderImage
func TestDocumentRenderImage(t *testing.T) {
	if os.Getenv("VORNIK_IMAGE_SMOKE") == "" {
		t.Skip("set VORNIK_IMAGE_SMOKE=1 for real agent-image rendering")
	}
	image := os.Getenv("VORNIK_AGENT_IMAGE")
	if image == "" {
		image = "ghcr.io/easeit-cz/vornik-agent:latest"
	}
	ws := t.TempDir()
	helper := filepath.Join(t.TempDir(), "helper")
	build := exec.Command("go", "build", "-o", helper, "../../cmd/agent-helper")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v %s", err, out)
	}
	const text = "příliš žluťoučký kůň čřžšýáíéúůě"
	mustWrite(t, filepath.Join(ws, "artifacts/in/report.md"), "# Praha\n\n"+text+`

![external](file:///fixtures/secret.svg)
![<img src="x" onerror="CAPTION_CANARY">](data:image/svg+xml,unsafe)

<script>SCRIPT_CANARY</script>
<iframe src="javascript:alert(1)"></iframe>
<style>@import url("http://169.254.169.254/STYLE_CANARY");</style>
<img src="http://169.254.169.254/secret" onerror="EVENT_CANARY">
<svg><a href="javascript:alert(1)">SVG_CANARY</a></svg>

[bad](javascript:alert%281%29) [bad](data:text/html,unsafe) [bad](vbscript:evil)
[local](file:///etc/passwd) [relative](../secret) [scheme relative](//example.invalid/evil)
[fragment](#praha) [http](http://example.invalid/safe) [https](https://example.invalid/safe) [mail](mailto:test@example.invalid)

broken <img src=x onerror="MALFORMED_CANARY"
`)
	if err := os.Chmod(filepath.Join(ws, "artifacts/in/report.md"), 0444); err != nil {
		t.Fatal(err)
	}
	base := []string{"run", "--rm", "--pull", "never", "--network", "none", "--memory", "512m", "--cpus", "1", "--pids-limit", "64", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--userns", "keep-id", "-v", ws + ":/app/workspace:Z", "-v", helper + ":/helper:ro,Z", "-e", "WORKSPACE=/app/workspace", "--entrypoint", "/helper", image}
	for _, format := range []string{"html", "pdf"} {
		args := append(append([]string{}, base...), "exec-tool", "document_render", `{"path":"artifacts/in/report.md","format":"`+format+`"}`)
		out, err := exec.Command("podman", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("container render: %v %s", err, out)
		}
		var result documentRenderResult
		if err := json.Unmarshal(out, &result); err != nil || !result.OK {
			t.Fatalf("render failed: %s %v", out, err)
		}
		data, err := os.ReadFile(filepath.Join(ws, result.Path))
		if err != nil || len(data) != result.Bytes {
			t.Fatalf("artifact mismatch: %v %s", err, out)
		}
		if format == "html" {
			body := strings.ToLower(string(data))
			for _, unsafe := range []string{"<script", "<iframe", "<img", "<svg", `href="javascript:`, `href="vbscript:`, `href="data:`, `href="file:`, `href="//`, "169.254.169.254", "script_canary", "event_canary", "style_canary", "svg_canary"} {
				if strings.Contains(body, unsafe) {
					t.Fatalf("unsafe HTML survived: %s in %s", unsafe, data)
				}
			}
			for _, expected := range []string{text, `href="#praha"`, `href="http://example.invalid/safe"`, `href="https://example.invalid/safe"`, `href="mailto:test@example.invalid"`} {
				if !strings.Contains(string(data), expected) {
					t.Fatalf("safe text/link lost: %s in %s", expected, data)
				}
			}
		}
		if format == "pdf" && !strings.HasPrefix(string(data), "%PDF-") {
			t.Fatal("not a PDF")
		}
	}
	// Exercise the real entrypoint role allowlist, not just helper Dispatch.
	for _, name := range []string{"entrypoint.sh", "tool_registry.generated.sh"} {
		data, err := os.ReadFile(filepath.Join("../../images/vornik-agent", name))
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(ws, name), string(data))
	}
	for _, granted := range []bool{false, true} {
		permissions := `{"config":{"permissions":{"allowedTools":[]}}}`
		if granted {
			permissions = `{"config":{"permissions":{"allowedTools":["document_render"]}}}`
		}
		mustWrite(t, filepath.Join(ws, "task.json"), permissions)
		gateBase := []string{"run", "--rm", "--pull", "never", "--network", "none", "--userns", "keep-id", "-v", ws + ":/app/workspace:Z", "-v", helper + ":/usr/local/bin/vornik-agent-helper:ro,Z", "-e", "WORKSPACE=/app/workspace", "-e", "INPUT_FILE=/app/workspace/task.json", "--entrypoint", "/bin/bash", image, "-c", `source /app/workspace/entrypoint.sh; trap - EXIT; exec_tool document_render '{"path":"artifacts/in/report.md","format":"html","out_path":"artifacts/out/gated.html"}'`}
		out, err := exec.Command("podman", gateBase...).CombinedOutput()
		if err != nil {
			t.Fatalf("entrypoint gate: %v %s", err, out)
		}
		if !granted {
			if !strings.Contains(string(out), "not allowed for this role") {
				t.Fatalf("ungranted tool reached converter: %s", out)
			}
			if _, err := os.Stat(filepath.Join(ws, "artifacts/out/gated.html")); !os.IsNotExist(err) {
				t.Fatalf("ungranted tool wrote output: %v", err)
			}
		} else {
			var result documentRenderResult
			if err := json.Unmarshal(out, &result); err != nil || !result.OK {
				t.Fatalf("granted tool failed: %s %v", out, err)
			}
		}
	}
	pdf := filepath.Join(ws, "artifacts/out/report.pdf")
	textOut, err := exec.Command("pdftotext", pdf, "-").CombinedOutput()
	if err != nil || !strings.Contains(string(textOut), text) {
		t.Fatalf("PDF Unicode text lost: %v %q", err, textOut)
	}
	// Directly exercise the production wrapper with adversarial HTML too.
	// A file SVG would visibly expose the canary if the deny-all fetcher regressed.
	mustWrite(t, filepath.Join(ws, "secret.svg"), `<svg xmlns="http://www.w3.org/2000/svg" width="400" height="60"><text x="0" y="25">RENDER_LOCAL_FILE_CANARY</text></svg>`)
	mustWrite(t, filepath.Join(ws, "wrapper.py"), documentPDFScript)
	const harness = `import io, sys
import weasyprint.urls
attempts = []
def forbidden(*args, **kwargs):
    attempts.append(True)
    raise AssertionError("network fetch attempted")
weasyprint.urls.urlopen = forbidden
sys.stdin = io.StringIO('<html><body>safe text<img src="file:///app/workspace/secret.svg"><img src="http://169.254.169.254/secret"><style>@import url("file:///etc/passwd");</style></body></html>')
ns = {}
exec(open('/app/workspace/wrapper.py').read(), ns)
assert not attempts, 'underlying network fetch reached'
for url in ['http://example.invalid/a', 'https://example.invalid/a', 'file:///etc/passwd', 'data:image/svg+xml,x', 'ftp://example.invalid/a']:
    try:
        ns['deny_url'](url)
    except ValueError:
        pass
    else:
        raise AssertionError('URL was not refused')
`
	args := []string{"run", "--rm", "--pull", "never", "--network", "none", "--userns", "keep-id", "-v", ws + ":/app/workspace:Z", "--entrypoint", "/usr/bin/python3", image, "-I", "-c", harness}
	cmd := exec.Command("podman", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(output), "%PDF-") {
		t.Fatalf("adversarial wrapper: %v %s", err, stderr.String())
	}
	mustWrite(t, filepath.Join(ws, "adversarial.pdf"), string(output))
	extracted, err := exec.Command("pdftotext", filepath.Join(ws, "adversarial.pdf"), "-").CombinedOutput()
	if err != nil || strings.Contains(string(extracted), "RENDER_LOCAL_FILE_CANARY") || !strings.Contains(string(extracted), "safe text") {
		t.Fatalf("resource escaped wrapper: %v %q", err, extracted)
	}
}
