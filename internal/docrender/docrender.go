// Package docrender holds the reviewed constants every document renderer
// shares: the agent's document_render (internal/agentloop) and the chat's
// render_document (internal/dispatcher). One sanitizer, so a fix to one
// renderer cannot miss the other.
//
// Design: https://docs.vornik.io
// §0.1 (the filter and the wrapper, #71) and §0.3 (shared, #339).
package docrender

// PandocFrom is the only markdown reader either renderer uses: GitHub-flavoured
// markdown with YAML front matter OFF. Front matter sets template variables
// (css, header-includes, include-before), which is how a document would put a
// stylesheet link into the output; with the extension off it is plain text.
// audit 2026-10-12 (#339): `css: //host/x.css` front matter put
// <link rel="stylesheet" href="//host/x.css"> into document_render's HTML.
const PandocFrom = "gfm-yaml_metadata_block"

// LuaFilter is applied by pandoc in order. Raw HTML/TeX is dropped, an image
// becomes its escaped alt text (never a resource), and a link keeps its target
// only for fragment, http(s) and mailto schemes.
const LuaFilter = `local function drop(_) return {} end
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

// WeasyPrintPrelude is the one deny-all URL fetcher and font stylesheet both
// PDF programs embed (review-20261012-b33c F1): WeasyPrint must never fetch
// any URL, even a local file or data URI.
const WeasyPrintPrelude = `from weasyprint import HTML, CSS
def deny_url(url, *args, **kwargs):
    raise ValueError("external resources are disabled")
css = CSS(string="body { font-family: 'DejaVu Sans', sans-serif; }")
`

// WeasyPrintScript renders HTML on stdin to PDF on stdout (document_render).
const WeasyPrintScript = `import sys
` + WeasyPrintPrelude + `document = HTML(string=sys.stdin.read(), url_fetcher=deny_url)
sys.stdout.buffer.write(document.write_pdf(stylesheets=[css]))
`

// Sandbox paths of render_document's one-shot (sandboxtool feature render).
// The daemon writes every /in file; none is named by the user.
const (
	SandboxInput  = "/in/input.md"
	SandboxMeta   = "/in/meta.yaml"
	SandboxFilter = "/in/sanitize.lua"
	SandboxPDFOut = "/out/output.pdf"
)

// SandboxPandocArgs is pandoc's argv inside the sandbox, before the output
// options: the input, the sanitizing reader and filter, the daemon's metadata.
func SandboxPandocArgs() []string {
	return []string{SandboxInput, "--from=" + PandocFrom, "--lua-filter=" + SandboxFilter, "--metadata-file=" + SandboxMeta}
}

// SandboxPDFScript is render_document's PDF program, run as `python3 -I -c`
// inside the sandbox: pandoc to standalone HTML with the shared reader and
// filter, then WeasyPrint with a deny-all URL fetcher. It replaces
// `pandoc --pdf-engine=weasyprint`, whose default fetcher reads file: URLs
// (audit 2026-10-12, #339: a file: image rendered into the PDF).
const SandboxPDFScript = `import subprocess, sys
` + WeasyPrintPrelude + `argv = ["/usr/bin/pandoc", "` + SandboxInput + `", "--from=` + PandocFrom + `", "--lua-filter=` + SandboxFilter + `",
        "--metadata-file=` + SandboxMeta + `", "--to=html5", "--standalone"]
run = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
if run.returncode != 0:
    sys.stderr.write(run.stderr.decode("utf-8", "replace"))
    sys.exit(run.returncode)
document = HTML(string=run.stdout.decode("utf-8"), url_fetcher=deny_url)
document.write_pdf("` + SandboxPDFOut + `", stylesheets=[css])
`
