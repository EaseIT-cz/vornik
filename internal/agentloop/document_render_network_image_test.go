package agentloop

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// audit 2026-10-12 (#339): document_render inside an agent container that HAS
// a network, with a capture server listening. The real helper renders the
// adversarial fixture; no request may arrive, and front matter may not put a
// stylesheet link into the delivered HTML (before the fix:
// <link rel="stylesheet" href="//127.0.0.1:<port>/front-matter.css">). The
// render succeeding under the production agent flags is also the proof that
// the renderer children's user+network namespace is available there.
//
//	VORNIK_IMAGE_SMOKE=1 go test ./internal/agentloop -run TestDocumentRenderImageWithNetwork -count=1 -v
func TestDocumentRenderImageWithNetwork(t *testing.T) {
	if os.Getenv("VORNIK_IMAGE_SMOKE") == "" {
		t.Skip("set VORNIK_IMAGE_SMOKE=1 for real agent-image rendering")
	}
	image := os.Getenv("VORNIK_AGENT_IMAGE")
	if image == "" {
		image = "ghcr.io/easeit-cz/vornik-agent:latest"
	}
	var mu sync.Mutex
	var requests []string
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><text>REMOTE</text></svg>`))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	h := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	ws := t.TempDir()
	helper := filepath.Join(t.TempDir(), "helper")
	if out, err := exec.Command("go", "build", "-o", helper, "../../cmd/agent-helper").CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v %s", err, out)
	}
	mustWrite(t, filepath.Join(ws, "artifacts/in/adv.md"), strings.Join([]string{
		"---", "css: //" + h + "/front-matter.css", "header-includes: <link rel=\"stylesheet\" href=\"http://" + h + "/fm-link.css\">", "---", "",
		"# Adversarial", "", "plain text survives", "",
		"inline math $x^2$ and display math", "", "$$\\int_0^1 f$$", "",
		"![md-remote](http://" + h + "/md-image.png)", "",
		`<img src="http://` + h + `/raw-img">`, "",
		`<iframe src="file:///etc/passwd"></iframe>`, "",
		"![passwd](/etc/passwd)", "",
		`<script>fetch("http://` + h + `/js")</script>`, "",
		`<style>@import url("http://` + h + `/import.css"); body { background: url("http://169.254.169.254/x"); } p { background: url("http://` + h + `/css-url"); }</style>`, "",
		`<link rel="stylesheet" href="http://` + h + `/link.css">`, "",
		`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="http://` + h + `/svg-ext"/></svg>`, "",
	}, "\n"))
	for _, format := range []string{"html", "pdf"} {
		// --network=host: the renderer's container can reach the capture
		// server on loopback, as an agent with egress can reach the world.
		args := []string{"run", "--rm", "--pull", "never", "--network", "host", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--userns", "keep-id", "-v", ws + ":/app/workspace:Z", "-v", helper + ":/helper:ro,Z", "-e", "WORKSPACE=/app/workspace",
			"--entrypoint", "/helper", image, "exec-tool", "document_render", `{"path":"artifacts/in/adv.md","format":"` + format + `"}`}
		out, err := exec.Command("podman", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("container render: %v %s", err, out)
		}
		var result documentRenderResult
		if err := json.Unmarshal(out, &result); err != nil || !result.OK {
			t.Fatalf("%s render failed: %s %v", format, out, err)
		}
		if format == "html" {
			body, err := os.ReadFile(filepath.Join(ws, result.Path))
			if err != nil {
				t.Fatal(err)
			}
			lower := strings.ToLower(string(body))
			// Front matter now renders as inert text, so the capture address
			// may appear as words; no attribute may point at it.
			for _, unsafe := range []string{`href="//`, `href="http://` + h, `src="`, "<script", "<iframe", "<img", "<svg", "<style>@import"} {
				if strings.Contains(lower, strings.ToLower(unsafe)) {
					t.Errorf("delivered HTML carries %q", unsafe)
				}
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("the renderer made requests: %v", requests)
	}
}
