package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// document_render runs only inside the agent image. The daemon's config
// assistant explicitly excludes this handler from its in-process envelope.
// Design: 2026-08-06-agent-document-render-tool-design.md §0.1 (#71).
func init() { Handlers["document_render"] = documentRender }

const (
	documentSourceCap = 256 * 1024
	documentOutputCap = 16 * 1024 * 1024
	documentErrorCap  = 2048
	documentPathCap   = 512
)

// This literal wrapper is the only Python program the tool can invoke.
// WeasyPrint must never fetch any URL, even a local file or data URI.
const documentPDFScript = `import sys
from weasyprint import HTML, CSS
def deny_url(url, *args, **kwargs):
    raise ValueError("external resources are disabled")
document = HTML(string=sys.stdin.read(), url_fetcher=deny_url)
css = CSS(string="body { font-family: 'DejaVu Sans', sans-serif; }")
sys.stdout.buffer.write(document.write_pdf(stylesheets=[css]))
`

// Pandoc applies these filters in order. Image captions become escaped text,
// never raw HTML or a new link/resource node.
const documentHTMLFilter = `local function drop(_) return {} end
local function safe_link(el)
  local target = el.target:lower()
  if target:match("^#") or target:match("^https?://") or target:match("^mailto:") then
    return el
  end
  return el.content
end
return {
  {RawBlock = drop, RawInline = drop, Link = safe_link},
  {Image = function(el) return pandoc.Str(pandoc.utils.stringify(el.caption)) end}
}
`

type documentRenderArgs struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	OutPath string `json:"out_path"`
}

type documentRenderResult struct {
	Path   string `json:"path,omitempty"` // first; result stays below audit cap
	OK     bool   `json:"ok"`
	Format string `json:"format,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Error  string `json:"error,omitempty"`
}

func documentResult(r documentRenderResult) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func documentFailure(format, stage string, err error) string {
	if format != "pdf" && format != "html" {
		format = ""
	}
	message := strings.ToValidUTF8(err.Error(), "")
	if len(message) > documentErrorCap {
		message = strings.ToValidUTF8(message[:documentErrorCap], "")
	}
	// Bound JSON-escaped bytes as well: control characters expand sixfold.
	for encoded, _ := json.Marshal(message); len(encoded) > documentErrorCap; encoded, _ = json.Marshal(message) {
		_, size := utf8.DecodeLastRuneInString(message)
		message = message[:len(message)-size]
	}
	return documentResult(documentRenderResult{Format: format, Stage: stage, Error: message})
}

type documentRenderer struct {
	command func(context.Context, string, ...string) *exec.Cmd
	timeout time.Duration
}

func documentRender(env Env, raw json.RawMessage) string {
	return renderDocumentWith(env, raw, documentRenderer{command: exec.CommandContext, timeout: 120 * time.Second})
}

func renderDocumentWith(env Env, raw json.RawMessage, renderer documentRenderer) string {
	var a documentRenderArgs
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return documentFailure("", "arguments", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return documentFailure(a.Format, "arguments", fmt.Errorf("arguments must contain one JSON object"))
	}
	if a.Path == "" || (a.Format != "pdf" && a.Format != "html") {
		return documentFailure(a.Format, "arguments", fmt.Errorf("path and format (pdf or html) are required"))
	}
	root, src, dst, err := documentPaths(env.Workspace, a)
	if err != nil {
		return documentFailure(a.Format, "resolve", err)
	}
	defer func() { _ = root.Close() }()
	content, err := readDocumentSource(root, src)
	if err != nil {
		return documentFailure(a.Format, "read", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), renderer.timeout)
	defer cancel()
	// The filter path is helper-owned, outside the workspace and never supplied
	// by the caller. Its contents are a reviewed literal, not document input.
	filterDir, err := os.MkdirTemp("", "vornik-document-filter-")
	if err != nil {
		return documentFailure(a.Format, "filter", err)
	}
	defer func() { _ = os.RemoveAll(filterDir) }()
	filterPath := filepath.Join(filterDir, "sanitize.lua")
	if err := os.WriteFile(filterPath, []byte(documentHTMLFilter), 0600); err != nil {
		return documentFailure(a.Format, "filter", err)
	}
	html, err := renderer.run(ctx, "/usr/bin/pandoc", []string{"--from=gfm", "--to=html5", "--standalone", "--lua-filter=" + filterPath}, content)
	if err != nil {
		return documentFailure(a.Format, "pandoc", err)
	}
	output := html
	if a.Format == "pdf" {
		output, err = renderer.run(ctx, "/usr/bin/python3", []string{"-I", "-c", documentPDFScript}, html)
		if err != nil {
			return documentFailure(a.Format, "weasyprint", err)
		}
		if !bytes.HasPrefix(output, []byte("%PDF-")) {
			return documentFailure(a.Format, "weasyprint", fmt.Errorf("renderer produced invalid PDF"))
		}
	}
	if len(output) == 0 {
		return documentFailure(a.Format, "render", fmt.Errorf("renderer produced empty output"))
	}
	if err := writeRenderedDocument(root, dst, output); err != nil {
		return documentFailure(a.Format, "write", err)
	}
	return documentResult(documentRenderResult{Path: filepath.ToSlash(dst), OK: true, Format: a.Format, Bytes: len(output)})
}

func documentPaths(workspace string, a documentRenderArgs) (*os.Root, string, string, error) {
	if strings.ToLower(filepath.Ext(a.Path)) != ".md" {
		return nil, "", "", fmt.Errorf("source must be a markdown (.md) file")
	}
	resolved, err := resolvePath(workspace, a.Path)
	if err != nil {
		return nil, "", "", err
	}
	if strings.HasPrefix(resolved, toolResultsDir(workspace)) {
		return nil, "", "", fmt.Errorf(".tool_results is only readable through tool_result_read")
	}
	dst := a.OutPath
	if dst == "" {
		dst = filepath.Join("artifacts", "out", strings.TrimSuffix(filepath.Base(a.Path), filepath.Ext(a.Path))+"."+a.Format)
	}
	dst = filepath.Clean(dst)
	if filepath.IsAbs(dst) || !strings.HasPrefix(dst, "artifacts"+string(os.PathSeparator)+"out"+string(os.PathSeparator)) || filepath.Ext(dst) != "."+a.Format || len(dst) > documentPathCap {
		return nil, "", "", fmt.Errorf("out_path must be under artifacts/out/, have the requested format extension, and be at most %d bytes", documentPathCap)
	}
	root, src, err := workspaceRoot(workspace, resolved)
	return root, src, dst, err
}

func readDocumentSource(root *os.Root, src string) ([]byte, error) {
	f, err := openDocumentSource(root, src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("source must be a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(f, documentSourceCap+1))
	if err != nil {
		return nil, err
	}
	if len(content) > documentSourceCap || !utf8.Valid(content) {
		return nil, fmt.Errorf("source must be valid UTF-8 and at most %d bytes", documentSourceCap)
	}
	return content, nil
}

// boundedDocumentBuffer drains a child without retaining unbounded output.
type boundedDocumentBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedDocumentBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (r documentRenderer) run(ctx context.Context, program string, args []string, input []byte) ([]byte, error) {
	cmd := r.command(ctx, program, args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(input)
	stdout := &boundedDocumentBuffer{limit: documentOutputCap}
	stderr := &boundedDocumentBuffer{limit: documentErrorCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second // inherited pipe holders cannot outlive timeout indefinitely
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	if stdout.overflow {
		return nil, fmt.Errorf("rendered output exceeds %d bytes", documentOutputCap)
	}
	return stdout.buffer.Bytes(), nil
}
