package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/sandboxtool"
)

// renderDocumentArgs is the parsed shape of the render_document tool
// arguments. See the tool descriptor for the documented fields.
type renderDocumentArgs struct {
	Content string   `json:"content"`
	Name    string   `json:"name"`
	Formats []string `json:"formats"`
}

// renderDocument writes markdown content + converted forms (HTML / PDF)
// and delivers each file directly to the chat via FileSender. No
// LLM and no task — the conversion is one pandoc run inside the pinned
// agent image (sandboxRenderer on the sandboxtool runner; never on the
// daemon host, process-spawn law S4/S5a). The dispatcher's prompt
// instructs the LLM to prefer this tool over create_task whenever
// the user supplies the content themselves and just wants the
// formats rendered.
//
// Why this exists: pre-2026-05-18 the bot's CV renders went through
// create_task → adaptive workflow → research agent → writer agent.
// Two LLMs scoping work whose deterministic transform is one
// pandoc invocation. The agents routinely hallucinated their way
// through "writer didn't produce file" loops; the user got nothing.
// render_document is the deterministic escape hatch.
//
// Formats vocabulary:
//   - "md"   — writes content verbatim to <name>.md and delivers it
//   - "html" — pandoc --standalone, in the agent image
//   - "pdf"  — pandoc --pdf-engine=weasyprint, in the agent image
//   - "docx" — pandoc, in the agent image
//
// When the sandbox cannot render (no agent image configured, podman or the
// image absent, a tool missing from it) the tool reports "rendering not
// available in the agent image" plainly; there is no host or in-process
// fallback, and the dispatcher's prompt forbids inline-rendering.
//
// renderRequestedFormats renders the requested non-md formats from the source
// markdown, returning the produced file paths (md source first, always) and a
// per-format failure list. Extracted from renderDocument to keep it under the
// complexity ratchet.
func renderRequestedFormats(ctx context.Context, r sandboxRenderer, tmpDir, mdPath, safeName string, wantSet map[string]bool) (produced, failures []string) {
	produced = []string{mdPath} // md is the source — always available.
	for _, format := range []string{"html", "pdf", "docx"} {
		if !wantSet[format] {
			continue
		}
		outPath, err := renderPath(tmpDir, safeName, format)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", format, err))
			continue
		}
		if err := r.render(ctx, mdPath, outPath, safeName, format); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", format, err))
		} else {
			produced = append(produced, outPath)
		}
	}
	return produced, failures
}

// deliverRenderedFiles streams each produced file to the operator via the
// FileSender, returning the basenames delivered and per-file send/open errors.
// Extracted from renderDocument to keep it under the complexity ratchet.
func deliverRenderedFiles(ctx context.Context, fs FileSender, produced []string) (delivered, sendErrs []string) {
	for _, p := range produced {
		base := filepath.Base(p)
		f, oerr := os.Open(p)
		if oerr != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("%s: %v", base, oerr))
			continue
		}
		err := fs.SendArtifactFile(ctx, base, f, "Rendered "+base)
		_ = f.Close()
		if err != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("%s: %v", base, err))
			continue
		}
		delivered = append(delivered, base)
	}
	return delivered, sendErrs
}

func (te *ToolExecutor) renderDocument(ctx context.Context, argsJSON string, fs FileSender) ToolResult {
	var args renderDocumentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	if strings.TrimSpace(args.Content) == "" {
		return ToolResult{Content: "render_document: content is required (the markdown source to render)."}
	}
	if strings.TrimSpace(args.Name) == "" {
		return ToolResult{Content: "render_document: name is required (the base filename, no extension)."}
	}
	if len(args.Formats) == 0 {
		args.Formats = []string{"md", "html", "pdf"}
	}
	safeName, err := safepath.CleanFileName(args.Name)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: invalid name: %v", err)}
	}
	// Strip any extension the caller accidentally included.
	safeName = strings.TrimSuffix(safeName, filepath.Ext(safeName))
	if safeName == "" {
		return ToolResult{Content: "render_document: name is required (the base filename, no extension)."}
	}
	if fs == nil {
		return ToolResult{Content: "render_document: file sending is not configured."}
	}

	tmpDir, err := os.MkdirTemp("", "vornik-render-*")
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: tmpdir: %v", err)}
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mdPath, err := renderPath(tmpDir, safeName, "md")
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: %v", err)}
	}
	if err := os.WriteFile(mdPath, []byte(args.Content), 0o600); err != nil {
		return ToolResult{Content: fmt.Sprintf("render_document: write md: %v", err)}
	}

	wantSet := map[string]bool{}
	for _, f := range args.Formats {
		wantSet[strings.ToLower(strings.TrimSpace(f))] = true
	}

	produced, failures := renderRequestedFormats(ctx, te.sandbox(), tmpDir, mdPath, safeName, wantSet)

	// If the caller asked for md ONLY, produced is [mdPath] and we
	// deliver it. If md wasn't requested but other formats were, drop
	// the md from delivery.
	if !wantSet["md"] {
		produced = produced[1:]
	}

	if len(produced) == 0 {
		if len(failures) > 0 {
			return ToolResult{Content: fmt.Sprintf("render_document: every requested format failed: %s", strings.Join(failures, "; "))}
		}
		return ToolResult{Content: "render_document: no formats requested."}
	}

	delivered, sendErrs := deliverRenderedFiles(ctx, fs, produced)

	switch {
	case len(delivered) > 0 && len(sendErrs) == 0 && len(failures) == 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s", strings.Join(delivered, ", ")), Provenance: outputguard.ProvenanceFirstParty}
	case len(delivered) > 0 && len(sendErrs) == 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s. Some formats failed to render: %s", strings.Join(delivered, ", "), strings.Join(failures, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	case len(delivered) > 0:
		return ToolResult{Content: fmt.Sprintf("Delivered: %s. Delivery errors: %s", strings.Join(delivered, ", "), strings.Join(sendErrs, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	default:
		return ToolResult{Content: fmt.Sprintf("render_document: nothing delivered. render failures: %s; send errors: %s", strings.Join(failures, "; "), strings.Join(sendErrs, "; ")), Provenance: outputguard.ProvenanceFirstParty}
	}
}

// renderPath is where <name>.<ext> is written. name is already a cleaned file
// name; this is the second guard, so a name that ever slipped past the first
// still cannot write outside tmpDir (S4 review F5).
func renderPath(tmpDir, name, ext string) (string, error) {
	if _, err := safepath.CleanPathComponent(name); err != nil {
		return "", fmt.Errorf("output name: %w", err)
	}
	p, err := safepath.JoinUnder(tmpDir, name+"."+ext)
	if err != nil {
		return "", fmt.Errorf("output name: %w", err)
	}
	return p, nil
}

// errRenderUnavailable reports that the sandbox cannot render at all: no agent
// image configured, podman absent, the image not present locally, or pandoc or
// weasyprint missing from it. There is deliberately no fallback, neither host
// pandoc nor an in-process approximation (process-spawn law, S4).
var errRenderUnavailable = errors.New("rendering not available in the agent image")

// sandboxRenderer renders markdown with pandoc INSIDE the pinned agent image —
// the allowlisted PodmanAgent kind of the process-spawn law
// (https://docs.vornik.io, S4). The run
// itself — limits, hardening, timeout, concurrency, scratch and metrics — is
// the sandboxtool runner's, feature "render" (S5a). What this adds:
//   - one fixed entrypoint, pandoc, with an argv built only from the format;
//   - argv carries no user text: the content is /in/input.md and the title is
//     /in/meta.yaml, both written by the daemon, and the output name is fixed.
type sandboxRenderer struct {
	runner *sandboxtool.Runner
}

func (te *ToolExecutor) sandbox() sandboxRenderer {
	return sandboxRenderer{runner: te.sandboxRunner}
}

// pandocArgs is the whole of pandoc's argv for one format.
func pandocArgs(format string) []string {
	args := []string{"/in/input.md", "--metadata-file=/in/meta.yaml"}
	switch format {
	case "pdf":
		args = append(args, "--pdf-engine=weasyprint")
	case "html":
		args = append(args, "--standalone")
	}
	// docx needs neither: pandoc writes a complete Word document natively.
	return append(args, "-o", "/out/output."+format)
}

// render converts mdPath into outPath; format is "html", "pdf" or "docx".
func (r sandboxRenderer) render(ctx context.Context, mdPath, outPath, title, format string) error {
	if r.runner == nil {
		return fmt.Errorf("%w (no sandbox runner configured)", errRenderUnavailable)
	}
	content, err := os.ReadFile(mdPath) //nolint:gosec // the daemon's own temp file
	if err != nil {
		return err
	}
	// JSON is valid YAML, and json.Marshal escapes the title safely.
	meta, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return err
	}
	res, err := r.runner.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureRender,
		Entrypoint: "pandoc",
		Args:       pandocArgs(format),
		Inputs:     []sandboxtool.Input{{Name: "input.md", Data: content}, {Name: "meta.yaml", Data: meta}},
	})
	if err != nil {
		var re *sandboxtool.RunError
		if errors.As(err, &re) && errors.Is(err, sandboxtool.ErrNotAvailable) {
			return fmt.Errorf("%w (%s)", errRenderUnavailable, re.Detail)
		}
		return err // timed out, out of memory, input too large, or pandoc's own error
	}
	defer res.Close()
	return moveFile(filepath.Join(res.OutDir, "output."+format), outPath)
}

// moveFile renames, falling back to a copy: the runner's scratch root and the
// render temp dir may be on different filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	b, err := os.ReadFile(src) //nolint:gosec // the runner's own scratch
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
