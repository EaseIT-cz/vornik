// Package image implements the image extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 5).
//
// Approach: pure-Go header decode for dimensions + format (uses
// stdlib image package which already supports jpeg/png/gif), then
// OCR with tesseract in the pinned agent image through the sandbox
// runner (process-spawn law S5b,
// https://docs.vornik.io §7),
// never on the daemon host. When the sandbox or the image's tesseract
// is not available, the extractor degrades gracefully — operators
// still get an image-with-metadata entry indexed into memory, they
// just don't get the recognised text layer, and the section says why.
//
// Why no EXIF in v1: reading EXIF requires a separate dependency
// (stdlib's image/jpeg skips APP1 segments entirely). Most useful
// image content the operator uploads is whiteboards / screenshots
// / diagrams; for those, OCR text is far more valuable than camera
// metadata. EXIF is a Phase-5b lift.
//
// Why no vision-model captions: those require an LLM call per
// image. The design doc puts that behind a per-project opt-in
// budget (§11 Phase 5 bullet 3); Phase-5 ships the deterministic
// floor only.
package image

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // stdlib decoder side-effect import
	_ "image/jpeg" // stdlib decoder side-effect import
	_ "image/png"  // stdlib decoder side-effect import
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
)

const (
	Name    = "vornik-extract-image"
	Version = "0.1.0"

	// maxImageBytes caps the per-image read. 32 MiB covers any
	// reasonable scanned/photographed input; a multi-GB raw image
	// from a DSLR is rejected here so the daemon doesn't OOM on
	// stdlib image.DecodeConfig (which fully reads the header
	// before returning size).
	maxImageBytes = 32 << 20
)

// New returns an image extractor that OCRs through sb. A nil sb, or an
// image without tesseract, is a non-fatal degradation: extraction still
// produces a metadata-only section.
//
// One image is one OCR "page", and one sandbox run: the image_ocr timeout
// (sandbox_tools.timeouts.image_ocr, default 120 s) is the per-page bound
// that batch-3 ingress hardening (d) introduced, so a crafted image that
// makes tesseract spin is capped per page, not per document.
func New(sb sandboxtool.Sandbox) *Extractor { return &Extractor{sandbox: sb} }

// Extractor implements extractor.Extractor for image files.
type Extractor struct {
	sandbox sandboxtool.Sandbox
}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract reads the image at src.FilePath, decodes its dimensions
// via stdlib (jpeg/png/gif), and optionally OCRs it with
// tesseract. Returns a single-section Result whose content is
// human-readable text the chunker + embedder both work cleanly
// with.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("image: source file path is empty")
	}

	f, err := os.Open(src.FilePath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Stat first so an enormous file fails fast instead of
	// streaming gigabytes through image.DecodeConfig.
	st, err := f.Stat()
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: stat: %w", err)
	}
	if st.Size() > maxImageBytes {
		return extractor.Result{}, fmt.Errorf("image: file size %d exceeds cap %d", st.Size(), maxImageBytes)
	}

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: decode header (unsupported format or corrupt file): %w", err)
	}

	title := titleFromSource(src)
	body := strings.Builder{}
	fmt.Fprintf(&body, "Image: %s\n", title)
	fmt.Fprintf(&body, "Format: %s\n", format)
	fmt.Fprintf(&body, "Dimensions: %d × %d pixels\n", cfg.Width, cfg.Height)
	fmt.Fprintf(&body, "File size: %d bytes\n", st.Size())

	// OCR pass — best-effort: a failed or unavailable OCR still
	// yields the metadata section, with a footer saying why.
	ocrText, ocrErr := e.runOCR(ctx, src.FilePath)
	switch {
	case ocrErr == nil && ocrText != "":
		body.WriteString("\n## Recognised text (tesseract OCR)\n\n")
		body.WriteString(ocrText)
	case errors.Is(ocrErr, sandboxtool.ErrNotAvailable):
		body.WriteString("\n(OCR not available in the agent image: " + ocrErr.Error() + ")")
	case ocrErr != nil:
		fmt.Fprintf(&body, "\n(OCR failed: %s)", ocrErr.Error())
	default:
		// ocrErr == nil && ocrText == "" — OCR ran but produced
		// no text. Common for icon-like images.
		body.WriteString("\n(OCR ran but recognised no text)")
	}

	content := strings.TrimSpace(body.String())
	section := extractor.Section{
		SectionID: "001-image",
		Title:     title,
		Content:   content,
	}
	outline := extractor.OutlineEntry{
		SectionID: section.SectionID,
		Title:     title,
		Depth:     0,
		TextBytes: len(content),
	}

	return extractor.Result{
		Metadata: extractor.Metadata{
			Title: title,
			Extra: map[string]string{
				"format":     format,
				"width":      fmt.Sprintf("%d", cfg.Width),
				"height":     fmt.Sprintf("%d", cfg.Height),
				"size_bytes": fmt.Sprintf("%d", st.Size()),
				"ocr_engine": ocrEngineLabel(ocrErr),
			},
		},
		Outline:  []extractor.OutlineEntry{outline},
		Sections: []extractor.Section{section},
	}, nil
}

// runOCR runs tesseract on the image in the sandbox and returns the
// recognised text. An error that errors.Is sandboxtool.ErrNotAvailable is a
// non-fatal degradation the caller reports as such.
func (e *Extractor) runOCR(ctx context.Context, imagePath string) (string, error) {
	if e.sandbox == nil {
		return "", fmt.Errorf("tesseract: %w (no sandbox runner)", sandboxtool.ErrNotAvailable)
	}
	// tesseract <input> <outputbase> writes <outputbase>.txt. --psm 3 is
	// the default page-segmentation mode (fully automatic, no OSD); we
	// keep it explicit so a future per-project tuning surface has a clean
	// knob. The input name is fixed: the operator's file name never
	// reaches argv.
	res, err := e.sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureImageOCR,
		Entrypoint: "tesseract",
		Args:       []string{"/in/image", "/out/ocr", "--psm", "3"},
		Inputs:     []sandboxtool.Input{{Name: "image", Path: imagePath}},
	})
	if err != nil {
		return "", fmt.Errorf("tesseract: %w", err)
	}
	defer res.Close()
	text, err := os.ReadFile(filepath.Join(res.OutDir, "ocr.txt"))
	if err != nil {
		return "", fmt.Errorf("tesseract wrote no text: %w", err)
	}
	return strings.TrimSpace(string(text)), nil
}

// ocrEngineLabel renders a small "ocr_engine" metadata tag the
// document-detail UI can surface. Lets the operator see at a
// glance which images got OCR vs which didn't, without scrolling
// to find the "(OCR not available)" footer.
func ocrEngineLabel(ocrErr error) string {
	switch {
	case errors.Is(ocrErr, sandboxtool.ErrNotAvailable):
		return "none (tesseract not available in the agent image)"
	case ocrErr != nil:
		return "failed"
	default:
		return "tesseract"
	}
}

// titleFromSource derives the image title from the
// operator-visible filename, extension stripped. Falls back to
// "image" when no filename is available.
func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" && src.FilePath != "" {
		name = filepath.Base(src.FilePath)
	}
	if name == "" {
		return "image"
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}
