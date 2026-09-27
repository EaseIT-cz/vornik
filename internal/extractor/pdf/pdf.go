// Package pdf implements the PDF extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 3) — text-extractable PDFs only; OCR fallback for
// scanned PDFs lands with Phase 5.
//
// Approach: poppler's pdftotext, run in the pinned agent image through
// the sandbox runner (process-spawn law S5b,
// https://docs.vornik.io §7): an
// uploaded PDF is untrusted input, so it is parsed in a network-less,
// memory-bounded one-shot, never on the daemon host. With no sandbox, or
// an image that does not declare pdftotext, extraction reports "not
// available in the agent image"; there is no host fallback. Page
// boundaries are preserved via the form-feed (\x0c) bytes pdftotext emits
// by default, which we split on to produce one section per page.
//
// Why not a pure-Go library: pdfcpu/ledongthuc/dslipak all fall
// short on the long tail of PDFs in the wild — embedded fonts,
// CID encodings, content streams that use Tj/TJ operators with
// custom encoding maps. poppler has 20+ years of incremental
// fixes for those edge cases. The performance + correctness
// gap is wider than the convenience win of staying in-process.
package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
)

const (
	// Name is the canonical extractor identifier persisted on
	// every extracted_documents.extractor_name row. The fixed
	// "vornik-extract-pdf" matches the design-doc naming.
	Name = "vornik-extract-pdf"

	// Version follows semver-ish. Bump when extraction logic
	// changes meaningfully — e.g. when we add TOC-based section
	// promotion or OCR fallback.
	Version = "0.1.0"

	// maxPDFPages caps the per-document section count. A PDF
	// claiming 50,000 pages is either a poppler bug, a malicious
	// payload, or an actual book that needs operator follow-up
	// (split before ingestion). 5000 covers any real textbook
	// or research compilation.
	maxPDFPages = 5000

	// inputName and outputName are fixed: the operator's file name
	// never reaches the tool's argv.
	inputName  = "document.pdf"
	outputName = "text.txt"
)

// New returns a PDF extractor that runs pdftotext through sb. A nil sb
// makes every extraction "not available".
func New(sb sandboxtool.Sandbox) *Extractor { return &Extractor{sandbox: sb} }

// Extractor implements extractor.Extractor for PDF files via the
// poppler pdftotext binary in the sandbox. Stateless; safe across
// goroutines.
type Extractor struct {
	sandbox sandboxtool.Sandbox
}

// Name returns the canonical extractor name.
func (*Extractor) Name() string { return Name }

// Version returns the extractor version string.
func (*Extractor) Version() string { return Version }

// Extract runs pdftotext on the source file in the sandbox and splits the
// output on form-feed page boundaries to produce one section per page.
// Returns a structured Result with one Section per page; the outline
// mirrors sections 1:1 with PageStart populated for citation-friendly
// retrieval.
//
// Errors:
//   - no sandbox, or pdftotext not in the agent image → an error that
//     errors.Is sandboxtool.ErrNotAvailable.
//   - pdftotext failure, timeout or memory limit → the run's outcome,
//     with poppler's own message ("Syntax Error: ...").
//   - Zero text extracted (scanned PDF) → return ErrNoTextExtracted
//     so callers can route to the OCR fallback when it lands.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("pdf: source file path is empty")
	}
	if e.sandbox == nil {
		return extractor.Result{}, fmt.Errorf("pdf: pdftotext: %w (no sandbox runner)", sandboxtool.ErrNotAvailable)
	}

	// pdftotext flags:
	//   -enc UTF-8     — force UTF-8 output (default is sometimes ASCII)
	//   -nopgbrk       — NOT passed: we rely on the form-feed (\x0c)
	//                    chars between pages for section splits.
	//   -q             — suppress informational stderr noise; real
	//                    errors still surface via non-zero exit.
	res, err := e.sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeaturePDF,
		Entrypoint: "pdftotext",
		Args:       []string{"-enc", "UTF-8", "-q", "/in/" + inputName, "/out/" + outputName},
		Inputs:     []sandboxtool.Input{{Name: inputName, Path: src.FilePath}},
	})
	if err != nil {
		return extractor.Result{}, fmt.Errorf("pdf: pdftotext: %w", err)
	}
	defer res.Close()
	text, err := os.ReadFile(filepath.Join(res.OutDir, outputName))
	if err != nil {
		return extractor.Result{}, fmt.Errorf("pdf: pdftotext wrote no text: %w", err)
	}

	pages := splitPages(text)
	// pdftotext often emits a trailing \x0c after the last page,
	// yielding an empty final entry from the split. Strip trailing
	// all-whitespace pages so PageCount reflects real pages, not
	// the splitter's accounting.
	for len(pages) > 0 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 0 {
		return extractor.Result{}, ErrNoTextExtracted
	}
	if len(pages) > maxPDFPages {
		return extractor.Result{}, fmt.Errorf("pdf: %d pages exceeds cap %d — split source before ingest", len(pages), maxPDFPages)
	}

	metadata := buildMetadata(src.OriginalName)
	metadata.PageCount = len(pages)

	sections := make([]extractor.Section, 0, len(pages))
	outline := make([]extractor.OutlineEntry, 0, len(pages))
	nonEmpty := 0
	for i, page := range pages {
		text := strings.TrimSpace(page)
		if text == "" {
			continue
		}
		nonEmpty++
		sectionID := fmt.Sprintf("page-%04d", i+1)
		title := summarisePage(text, i+1)
		sections = append(sections, extractor.Section{
			SectionID: sectionID,
			Title:     title,
			Content:   text,
		})
		outline = append(outline, extractor.OutlineEntry{
			SectionID: sectionID,
			Title:     title,
			Depth:     0,
			PageStart: i + 1,
			TextBytes: len(text),
		})
	}
	if nonEmpty == 0 {
		// Every page parsed but produced no text — the canonical
		// scanned-PDF signal. Return a sentinel so future Phase-5
		// callers can re-route to OCR without re-parsing.
		return extractor.Result{Metadata: metadata}, ErrNoTextExtracted
	}

	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// ErrNoTextExtracted means the PDF parsed but contained no
// extractable text. Typical cause: scanned-image PDFs where the
// pages are bitmaps with no text layer. Caller-visible so the
// future OCR fallback can route to a different extractor.
var ErrNoTextExtracted = errors.New("pdf: no text content extracted (likely scanned/image PDF — OCR fallback not yet available)")

// splitPages divides pdftotext output on form-feed bytes (\x0c).
// pdftotext emits \x0c between pages by default; the last page
// has no trailing form-feed so we don't drop trailing empties.
//
// We return [][]byte so callers can inspect raw bytes; the section
// builder above converts to string after TrimSpace.
func splitPages(data []byte) []string {
	parts := bytes.Split(data, []byte{0x0c})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out
}

// summarisePage builds a short, deterministic section title from
// the page's leading text. PDFs don't carry per-page titles, so
// we take the first non-empty line, truncated to 80 chars. Falls
// back to "Page N" when the page starts with only whitespace.
//
// Quality matters: this title shows up in memory_search results
// + document_get_outline responses. A clean "Chapter 4 — Schema
// Mode Mapping" beats "[binary garbage]" or "Page 47".
func summarisePage(text string, pageNumber int) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > 80 {
			trimmed = trimmed[:80] + "…"
		}
		return trimmed
	}
	return fmt.Sprintf("Page %d", pageNumber)
}

// buildMetadata populates the fields PDF can offer up-front. Title
// falls back to the operator-visible filename (extension stripped)
// when the PDF doesn't carry document properties. Reading the
// embedded XMP / Info dictionary for the real title is a Phase-3b
// nice-to-have; for now the filename heuristic matches user
// expectation when forwarding "<paper-name>.pdf".
func buildMetadata(originalName string) extractor.Metadata {
	m := extractor.Metadata{}
	if originalName != "" {
		title := originalName
		if i := strings.LastIndex(title, "."); i > 0 {
			title = title[:i]
		}
		m.Title = title
	}
	return m
}
