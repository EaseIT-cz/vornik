// Tests for the image extractor. We generate fixture images
// inline (1×1 PNG / JPEG) so the happy path runs without
// committing binary blobs to the repo; OCR is exercised via
// a stub binary on operator-controlled PATH.
package image

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	swarmextractor "vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

func writePNGFixture(t *testing.T, w, h int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.png")
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func writeJPEGFixture(t *testing.T, w, h int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jpg")
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtract_PNG_HappyPath_NoOCR(t *testing.T) {
	// Force OCR-unavailable by pointing at a missing binary.
	ext := New(nil)
	path := writePNGFixture(t, 64, 32)
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/png",
		OriginalName: "diagram.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1", len(res.Sections))
	}
	s := res.Sections[0]
	if !strings.Contains(s.Content, "Format: png") {
		t.Errorf("section content missing format: %q", s.Content)
	}
	if !strings.Contains(s.Content, "64 × 32 pixels") {
		t.Errorf("dimensions not surfaced: %q", s.Content)
	}
	if !strings.Contains(s.Content, "OCR not available") {
		t.Errorf("missing OCR-unavailable note: %q", s.Content)
	}
	if res.Metadata.Title != "diagram" {
		t.Errorf("Title = %q; want \"diagram\"", res.Metadata.Title)
	}
	if res.Metadata.Extra["format"] != "png" {
		t.Errorf("metadata.format = %q", res.Metadata.Extra["format"])
	}
	if res.Metadata.Extra["ocr_engine"] != "none (tesseract not available in the agent image)" {
		t.Errorf("ocr_engine tag = %q", res.Metadata.Extra["ocr_engine"])
	}
}

func TestExtract_JPEG_DecodesDimensions(t *testing.T) {
	ext := New(nil)
	path := writeJPEGFixture(t, 200, 100)
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/jpeg",
		OriginalName: "photo.jpg",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Extra["format"] != "jpeg" {
		t.Errorf("format = %q; want jpeg", res.Metadata.Extra["format"])
	}
	if res.Metadata.Extra["width"] != "200" || res.Metadata.Extra["height"] != "100" {
		t.Errorf("dimensions metadata = %+v", res.Metadata.Extra)
	}
}

func TestExtract_NonImage_Errors(t *testing.T) {
	// Random bytes that aren't a valid image header.
	dir := t.TempDir()
	path := filepath.Join(dir, "junk.png")
	if err := os.WriteFile(path, []byte("this is not an image"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New(nil).Extract(context.Background(), swarmextractor.Source{FilePath: path})
	if err == nil {
		t.Fatal("expected error on non-image input")
	}
}

func TestExtract_EmptyPath_Errors(t *testing.T) {
	_, err := New(nil).Extract(context.Background(), swarmextractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

func TestExtract_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.png")
	if err := os.WriteFile(path, make([]byte, maxImageBytes+1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New(nil).Extract(context.Background(), swarmextractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected cap-exceeded error; got %v", err)
	}
}

// fakeTesseract plays tesseract in the sandbox: it writes text to the
// output base the argv names (tesseract appends .txt).
func fakeTesseract(t *testing.T, text string) *sandboxtest.Fake {
	return sandboxtest.New(t, func(_ sandboxtool.Spec, _ map[string][]byte, out string) error {
		return os.WriteFile(filepath.Join(out, "ocr.txt"), []byte(text), 0o600)
	})
}

// TestExtract_OCR_RunsInTheSandbox: tesseract's text is folded into the
// section, and the run is the fixed image_ocr shape (process-spawn law S5b).
func TestExtract_OCR_RunsInTheSandbox(t *testing.T) {
	sb := fakeTesseract(t, "WHITEBOARD: design sketch\n")
	path := writePNGFixture(t, 32, 32)
	res, err := New(sb).Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/png",
		OriginalName: "whiteboard.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	spec := sb.Specs()[0]
	if spec.Feature != sandboxtool.FeatureImageOCR || spec.Entrypoint != "tesseract" ||
		strings.Join(spec.Args, " ") != "/in/image /out/ocr --psm 3" ||
		len(spec.Inputs) != 1 || spec.Inputs[0].Name != "image" || spec.Inputs[0].Path != path {
		t.Fatalf("run = %+v", spec)
	}
	body0 := res.Sections[0].Content
	if !strings.Contains(body0, "## Recognised text (tesseract OCR)") {
		t.Errorf("missing OCR section header: %q", body0)
	}
	if !strings.Contains(body0, "WHITEBOARD: design sketch") {
		t.Errorf("OCR text not folded into content: %q", body0)
	}
	if res.Metadata.Extra["ocr_engine"] != "tesseract" {
		t.Errorf("ocr_engine = %q", res.Metadata.Extra["ocr_engine"])
	}
}

// TestExtract_OCR_StubProducesEmpty — when tesseract runs but
// returns no text (small icons / blank images), the extractor
// must still produce a valid section; the OCR footer should
// indicate "no text recognised" rather than failing the whole
// extraction.
func TestExtract_OCR_StubProducesEmpty(t *testing.T) {
	path := writePNGFixture(t, 16, 16)
	res, err := New(fakeTesseract(t, "\n")).Extract(context.Background(), swarmextractor.Source{
		FilePath: path, OriginalName: "icon.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Sections[0].Content, "OCR ran but recognised no text") {
		t.Errorf("empty-OCR footer missing: %q", res.Sections[0].Content)
	}
	if res.Metadata.Extra["ocr_engine"] != "tesseract" {
		t.Errorf("ocr_engine = %q", res.Metadata.Extra["ocr_engine"])
	}
}

// TestExtract_OCR_TimeoutDegradesHonestly — batch-3 ingress/untrusted-input
// hardening (d): one OCR run is bounded per page. The bound is now the
// sandbox's image_ocr timeout; a run it kills still yields the metadata
// section, with a footer saying OCR failed and why.
func TestExtract_OCR_TimeoutDegradesHonestly(t *testing.T) {
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return &sandboxtool.RunError{Outcome: sandboxtool.OutcomeTimeout, Feature: sandboxtool.FeatureImageOCR, Detail: "after 2m0s"}
	})
	res, err := New(sb).Extract(context.Background(), swarmextractor.Source{
		FilePath: writePNGFixture(t, 32, 32), OriginalName: "slow.png",
	})
	if err != nil {
		t.Fatalf("Extract should degrade gracefully on OCR timeout, got: %v", err)
	}
	body0 := res.Sections[0].Content
	if !strings.Contains(body0, "OCR failed") || !strings.Contains(body0, "timed out") {
		t.Errorf("expected an OCR-failed footer naming the timeout; got: %q", body0)
	}
	if res.Metadata.Extra["ocr_engine"] != "failed" {
		t.Errorf("ocr_engine = %q; want failed", res.Metadata.Extra["ocr_engine"])
	}
}

// §7.1 decision 4: an image without tesseract never falls back to the host.
func TestExtract_OCR_NotAvailableNeverRunsTheHostTool(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(filepath.Join(dir, "tesseract"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return sandboxtest.NotAvailable(sandboxtool.FeatureImageOCR)
	})
	res, err := New(sb).Extract(context.Background(), swarmextractor.Source{FilePath: writePNGFixture(t, 8, 8)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Sections[0].Content, "OCR not available in the agent image") {
		t.Errorf("footer: %q", res.Sections[0].Content)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a host tesseract ran")
	}
}

func TestTitleFromSource(t *testing.T) {
	cases := map[string]string{
		"photo.jpg":      "photo",
		"WHITEBOARD.PNG": "WHITEBOARD",
		"":               "image",
	}
	for in, want := range cases {
		got := titleFromSource(swarmextractor.Source{OriginalName: in})
		if got != want {
			t.Errorf("titleFromSource(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestExtractor_Identifies(t *testing.T) {
	e := New(nil)
	if e.Name() != Name {
		t.Errorf("Name = %q; want %q", e.Name(), Name)
	}
	if e.Version() != Version {
		t.Errorf("Version = %q; want %q", e.Version(), Version)
	}
}
