// Tests for the PDF extractor. pdftotext runs in the agent image through
// the sandbox runner (process-spawn law S5b, design §7), never on the daemon
// host, so these tests play it with a fake sandbox and pin the run's shape:
// feature, fixed entrypoint, fixed argv with no user text, the input copied
// in from its path, the text read back from /out. The real tool runs under
// the podman e2e lane (test/e2e/sandbox_media_test.go).
package pdf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// fakePDFToText plays pdftotext: it writes text to the output path the argv
// names.
func fakePDFToText(t *testing.T, text string) *sandboxtest.Fake {
	return sandboxtest.New(t, func(_ sandboxtool.Spec, _ map[string][]byte, out string) error {
		return os.WriteFile(filepath.Join(out, "text.txt"), []byte(text), 0o600)
	})
}

func writePDF(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report (final).pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4 fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtract_RunsPdftotextInTheSandbox(t *testing.T) {
	sb := fakePDFToText(t, "Hello, PDF world.\n\x0c")
	path := writePDF(t)
	res, err := New(sb).Extract(context.Background(), extractor.Source{
		FilePath: path, MimeType: "application/pdf", OriginalName: "hello.pdf",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	specs := sb.Specs()
	if len(specs) != 1 {
		t.Fatalf("want one sandbox run, got %d", len(specs))
	}
	spec := specs[0]
	if spec.Feature != sandboxtool.FeaturePDF || spec.Entrypoint != "pdftotext" {
		t.Fatalf("run = %s/%s", spec.Feature, spec.Entrypoint)
	}
	if got := strings.Join(spec.Args, " "); got != "-enc UTF-8 -q /in/document.pdf /out/text.txt" {
		t.Fatalf("argv = %q", got)
	}
	// The input is copied from its path under a fixed name: the operator's
	// file name never reaches argv.
	if len(spec.Inputs) != 1 || spec.Inputs[0].Name != "document.pdf" || spec.Inputs[0].Path != path {
		t.Fatalf("inputs = %+v", spec.Inputs)
	}
	if len(res.Sections) != 1 || !strings.Contains(res.Sections[0].Content, "Hello") {
		t.Fatalf("sections = %+v", res.Sections)
	}
	if res.Sections[0].SectionID != "page-0001" || res.Outline[0].PageStart != 1 || res.Metadata.PageCount != 1 {
		t.Fatalf("page accounting: %+v %+v", res.Sections[0], res.Metadata)
	}
	if res.Metadata.Title != "hello" {
		t.Errorf("Metadata.Title = %q; want \"hello\"", res.Metadata.Title)
	}
}

// §7.1 decision 4: no sandbox, or a tool the image does not declare, is "not
// available" — never a host fallback.
func TestExtract_NotAvailableNeverFallsBackToTheHost(t *testing.T) {
	marker := hostTrap(t, "pdftotext")
	_, err := New(nil).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)})
	if !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: want not available, got %v", err)
	}
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return sandboxtest.NotAvailable(sandboxtool.FeaturePDF)
	})
	_, err = New(sb).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)})
	if !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("undeclared tool: want not available, got %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("a host pdftotext ran")
	}
}

func TestExtract_EmptyFilePath(t *testing.T) {
	_, err := New(fakePDFToText(t, "x")).Extract(context.Background(), extractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

func TestExtract_PdftotextFailure_SurfacesTheToolsMessage(t *testing.T) {
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return sandboxtest.Failed(sandboxtool.FeaturePDF, "Syntax Error: Couldn't find trailer dictionary")
	})
	_, err := New(sb).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)})
	if err == nil || !strings.Contains(err.Error(), "pdftotext") || !strings.Contains(err.Error(), "trailer dictionary") {
		t.Fatalf("error should name pdftotext and carry its message; got %v", err)
	}
}

func TestExtract_ScannedPDFReportsNoText(t *testing.T) {
	_, err := New(fakePDFToText(t, "  \n\x0c \n\x0c")).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)})
	if !errors.Is(err, ErrNoTextExtracted) {
		t.Fatalf("want ErrNoTextExtracted, got %v", err)
	}
	_, err = New(fakePDFToText(t, "a\x0c \x0cb")).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)})
	if err != nil {
		t.Fatalf("a blank middle page is skipped, not an error: %v", err)
	}
	// No output file at all is a failure, not an empty document.
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error { return nil })
	if _, err := New(sb).Extract(context.Background(), extractor.Source{FilePath: writePDF(t)}); err == nil {
		t.Fatal("a missing output must be an error")
	}
}

// hostTrap puts a fake program first on PATH that leaves a marker if anything
// runs it on the host.
func hostTrap(t *testing.T, program string) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestSplitPages(t *testing.T) {
	// pdftotext emits \x0c between pages. Verify our splitter
	// produces N pages for N-1 form-feed separators.
	cases := []struct {
		in       string
		wantLen  int
		wantP0   string
		wantP1   string
		wantLast string
	}{
		{"page1", 1, "page1", "", "page1"},
		{"page1\x0cpage2", 2, "page1", "page2", "page2"},
		{"a\x0cb\x0cc", 3, "a", "b", "c"},
		{"\x0c", 2, "", "", ""}, // empty pages on either side
	}
	for i, c := range cases {
		pages := splitPages([]byte(c.in))
		if len(pages) != c.wantLen {
			t.Errorf("case %d: got %d pages, want %d", i, len(pages), c.wantLen)
			continue
		}
		if pages[0] != c.wantP0 {
			t.Errorf("case %d: pages[0] = %q, want %q", i, pages[0], c.wantP0)
		}
		if c.wantLen > 1 && pages[1] != c.wantP1 {
			t.Errorf("case %d: pages[1] = %q, want %q", i, pages[1], c.wantP1)
		}
		if pages[len(pages)-1] != c.wantLast {
			t.Errorf("case %d: pages[last] = %q, want %q", i, pages[len(pages)-1], c.wantLast)
		}
	}
}

func TestSummarisePage(t *testing.T) {
	cases := []struct {
		page, want string
		pageNum    int
	}{
		{"Chapter 4 — Schema Mode Mapping\n\nbody body body", "Chapter 4 — Schema Mode Mapping", 4},
		{"   \n   \n\nHello world\nmore text", "Hello world", 1},
		// Truncates a long heading + adds ellipsis.
		{strings.Repeat("x", 200) + "\nrest", strings.Repeat("x", 80) + "…", 7},
		// All whitespace falls back to "Page N".
		{"   \n  \n\n", "Page 12", 12},
	}
	for i, c := range cases {
		got := summarisePage(c.page, c.pageNum)
		if got != c.want {
			t.Errorf("case %d: summarisePage = %q, want %q", i, got, c.want)
		}
	}
}

func TestBuildMetadata_TitleFromFilename(t *testing.T) {
	cases := map[string]string{
		"paper.pdf":                "paper",
		"long.title.with.dots.pdf": "long.title.with.dots",
		"no_extension":             "no_extension",
		"":                         "",
	}
	for in, want := range cases {
		got := buildMetadata(in)
		if got.Title != want {
			t.Errorf("buildMetadata(%q).Title = %q; want %q", in, got.Title, want)
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
