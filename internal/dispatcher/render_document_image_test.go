package dispatcher

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/sandboxtool"
)

// render_document against the adversarial fixture of issue #339, in the real
// agent image through the production sandbox runner. Opt-in: it needs Podman
// and the image locally.
//
//	VORNIK_IMAGE_SMOKE=1 go test ./internal/dispatcher -run TestRenderDocumentImage -count=1 -v
//
// audit 2026-10-12 (#339): before the shared sanitizer the HTML kept <script>
// and <iframe>, the DOCX embedded /etc/passwd, /proc/self/mountinfo and
// /proc/self/environ as media, and the PDF rendered a file: image.
func TestRenderDocumentImage_AdversarialFixture(t *testing.T) {
	if os.Getenv("VORNIK_IMAGE_SMOKE") == "" {
		t.Skip("set VORNIK_IMAGE_SMOKE=1 for real agent-image rendering")
	}
	image := os.Getenv("VORNIK_AGENT_IMAGE")
	if image == "" {
		image = testRenderImage
	}
	runner, err := sandboxtool.New(sandboxtool.Config{Image: image, ScratchRoot: t.TempDir()}, sandboxtool.ExecCommand)
	if err != nil {
		t.Fatal(err)
	}
	te := &ToolExecutor{sandboxRunner: runner}
	fs := &imageSender{files: map[string][]byte{}}
	const png = "/usr/lib/python3/dist-packages/scipy/ndimage/tests/dots.png"
	content := strings.Join([]string{
		"---", "css: //127.0.0.1:9/front-matter.css", "header-includes: <script>FM_CANARY</script>", "---", "",
		"# Adversarial", "", "plain text survives", "",
		"![md-remote](http://127.0.0.1:9/md-image.png)", "",
		`<img src="http://169.254.169.254/latest/meta-data/">`, "",
		`<iframe src="file:///etc/passwd"></iframe>`, "",
		`<img src="file://` + png + `">`, "",
		"![png](" + png + ")", "",
		"![passwd](/etc/passwd)", "", "![mounts](/proc/self/mountinfo)", "", "![env](/proc/self/environ)", "",
		`<script>fetch("http://127.0.0.1:9/js"); document.write("SCRIPT_CANARY")</script>`, "",
		`<style>@import url("http://127.0.0.1:9/import.css"); body { background: url("http://169.254.169.254/x"); }</style>`, "",
		`<svg xmlns="http://www.w3.org/2000/svg"><image href="file:///etc/passwd"/></svg>`, "",
	}, "\n")
	quoted, _ := json.Marshal(content)
	args := `{"content":` + string(quoted) + `,"name":"adversarial","formats":["html","pdf","docx"]}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := te.renderDocument(ctx, args, fs)
	if !strings.Contains(res.Content, "Delivered: adversarial.html, adversarial.pdf, adversarial.docx") {
		t.Fatalf("render_document: %s", res.Content)
	}

	html := strings.ToLower(string(fs.files["adversarial.html"]))
	// Front matter now renders as inert text, so FM_CANARY may appear as
	// words; what must not appear is markup, a stylesheet link or a URL.
	for _, unsafe := range []string{"<script", "<iframe", "<img", "<svg", "<style>@import", "script_canary", `rel="stylesheet" href="//`, "169.254.169.254", "file://"} {
		if strings.Contains(html, unsafe) {
			t.Errorf("HTML carries %q", unsafe)
		}
	}
	if !strings.Contains(html, "plain text survives") {
		t.Error("HTML lost the document text")
	}

	docx := fs.files["adversarial.docx"]
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatalf("DOCX is not a zip: %v", err)
	}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "word/media/") {
			t.Errorf("DOCX embeds %s: a local file was read into the document", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		for _, leak := range []string{"root:x:0:0", " /out ", "container=podman"} {
			if bytes.Contains(b, []byte(leak)) {
				t.Errorf("DOCX part %s leaks %q", f.Name, leak)
			}
		}
	}

	pdf := fs.files["adversarial.pdf"]
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", firstN(pdf))
	}
	pdfPath := filepath.Join(t.TempDir(), "adversarial.pdf")
	if err := os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("pdfimages", "-list", pdfPath).CombinedOutput(); err == nil {
		// Header and rule are two lines; any further line is an embedded image.
		if lines := strings.Count(strings.TrimSpace(string(out)), "\n") + 1; lines > 2 {
			t.Errorf("PDF embeds an image read from the image filesystem:\n%s", out)
		}
	} else {
		t.Logf("pdfimages unavailable (%v): embedded-image check skipped", err)
	}
	if out, err := exec.Command("pdftotext", pdfPath, "-").CombinedOutput(); err == nil {
		text := string(out)
		if strings.Contains(text, "root:x:0:0") || !strings.Contains(text, "plain text survives") {
			t.Errorf("PDF text: %q", text)
		}
	}
}

type imageSender struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *imageSender) SendArtifactFile(_ context.Context, name string, r io.Reader, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = b
	return nil
}

func firstN(b []byte) []byte {
	if len(b) > 32 {
		return b[:32]
	}
	return b
}
