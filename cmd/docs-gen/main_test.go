package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func TestRenderCLI_EmitsCommandsAndFlags(t *testing.T) {
	root := &cobra.Command{Use: "vornikctl", Short: "root short"}
	sub := &cobra.Command{Use: "task", Short: "manage tasks", Run: func(*cobra.Command, []string) {}}
	sub.Flags().String("priority", "normal", "task priority")
	hidden := &cobra.Command{Use: "secret", Hidden: true, Run: func(*cobra.Command, []string) {}}
	internalCmd := &cobra.Command{Use: "admin", Short: "internal admin", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(sub, hidden, internalCmd)

	out := renderCLI(root, map[string]bool{"task": true})
	if !strings.Contains(out, "## vornikctl task") {
		t.Errorf("expected task command heading; got:\n%s", out)
	}
	if !strings.Contains(out, "`--priority`") || !strings.Contains(out, "task priority") {
		t.Errorf("expected priority flag row; got:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("hidden command must not appear; got:\n%s", out)
	}
	if strings.Contains(out, "## vornikctl admin") {
		t.Errorf("non-allowlisted top-level command must not appear; got:\n%s", out)
	}
}

func TestCollectLLMSSections_UsesMkdocsNavAndExcludesHiddenPages(t *testing.T) {
	cfg := `
site_url: https://docs.vornik.io
docs_dir: docs/public
exclude_docs: |
  features/trading-series.md
  drafts/**
nav:
  - Home: index.md
  - standalone.md
  - Getting Started:
      - getting-started/index.md
  - Features:
      - features/blackbox.md
      - features/trading-series.md
      - drafts/private.md
  - Troubleshooting: troubleshooting/index.md
`
	var parsed mkdocsConfig
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}

	sections, err := collectLLMSSections(parsed.Nav, parseExcludeDocs(parsed.ExcludeDocs))
	if err != nil {
		t.Fatal(err)
	}
	got := renderSectionSketch(sections)
	want := "Other:standalone.md\nGetting Started:getting-started/index.md\nFeatures:features/blackbox.md\nTroubleshooting:troubleshooting/index.md\n"
	if got != want {
		t.Fatalf("unexpected llms sections:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestParseExcludeDocs_AcceptsListFormAndGlobs(t *testing.T) {
	cfg := `
exclude_docs:
  - hidden.md
  - drafts/**
  - "*.tmp.md"
`
	var parsed mkdocsConfig
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}

	rules := parseExcludeDocs(parsed.ExcludeDocs)
	for _, path := range []string{"hidden.md", "drafts/private.md", "notes.tmp.md"} {
		if !rules.match(path) {
			t.Fatalf("expected %s to be excluded by %#v", path, rules)
		}
	}
	if rules.match("visible.md") {
		t.Fatalf("visible.md should not be excluded by %#v", rules)
	}
}

func TestRenderLLMsFromConfig_FailsWhenNavFileMissing(t *testing.T) {
	cfg := mkdocsConfig{DocsDir: "docs"}
	if err := yaml.Unmarshal([]byte(`- Missing: missing.md`), &cfg.Nav); err != nil {
		t.Fatal(err)
	}

	_, err := renderLLMsFromConfig(t.TempDir(), cfg)
	if err == nil || !strings.Contains(err.Error(), "missing.md") {
		t.Fatalf("expected missing nav file error, got %v", err)
	}
}

func TestRenderLLMsFromConfig_FailsWhenNavEntryHasNoTarget(t *testing.T) {
	cfg := mkdocsConfig{DocsDir: "docs"}
	if err := yaml.Unmarshal([]byte(`- Empty:`), &cfg.Nav); err != nil {
		t.Fatal(err)
	}

	_, err := renderLLMsFromConfig(t.TempDir(), cfg)
	if err == nil || !strings.Contains(err.Error(), `has no target`) {
		t.Fatalf("expected empty nav target error, got %v", err)
	}
}

func TestCollectLLMSPages_FailsOnUnmatchedMappingKey(t *testing.T) {
	node := yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "Broken"},
		},
	}

	_, err := collectLLMSPages(&node, nil)
	if err == nil || !strings.Contains(err.Error(), "unmatched key") {
		t.Fatalf("expected unmatched key error, got %v", err)
	}
}

func TestLoadMkdocsConfig_FailsWhenNavEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mkdocs.yml")
	if err := os.WriteFile(path, []byte("site_name: Empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadMkdocsConfig(path)
	if err == nil || !strings.Contains(err.Error(), "mkdocs nav is empty") {
		t.Fatalf("expected empty nav error, got %v", err)
	}
}

func TestRenderLLMsFromConfig_RendersStableURLsAndDescriptions(t *testing.T) {
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	if err := os.MkdirAll(filepath.Join(docs, "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "guides", "index.md"), []byte("# Guides\n\nTask-focused docs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mkdocsConfig{SiteURL: "https://docs.vornik.io/", DocsDir: "docs"}
	if err := yaml.Unmarshal([]byte(`- Guides:
    - guides/index.md
`), &cfg.Nav); err != nil {
		t.Fatal(err)
	}

	got, err := renderLLMsFromConfig(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Vornik\n\n" +
		"> Local-first orchestration daemon for teams of AI agents. Vornik runs asynchronous projects, swarms, workflows, memory, approvals, companion delegation, and operator tooling on infrastructure you control.\n\n" +
		"Install: `curl -fsSL https://get.vornik.io | bash`  (Linux with rootless Podman recommended)\n\n" +
		"Repo: https://github.com/EaseIT-cz/vornik\n" +
		"Docs: https://docs.vornik.io\n\n" +
		"## Guides\n" +
		"- [Guides](https://docs.vornik.io/guides/): Task-focused docs.\n"
	if got != want {
		t.Fatalf("unexpected llms output:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestPageDescription_SkipsFrontMatterAndAdmonitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.md")
	body := `---
sources:
  - path: internal/example.go
---
# Companion plugin

!!! note "Community Edition"

    This is a publishing note, not the page summary.

The vornik **companion** connects your host LLM session to a running vornik
daemon. See [Knowledge skills](knowledge-skills.md) and use delegate.

## Next
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := pageDescription(path)
	want := "The vornik companion connects your host LLM session to a running vornik daemon. See Knowledge skills and use delegate."
	if got != want {
		t.Fatalf("unexpected description:\nwant %q\n got %q", want, got)
	}
}

func TestPageDescription_SkipsFencedCodeBeforeSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.md")
	body := "# Quickstart\n\n```bash\nmake run\n```\n\nThe real one-line summary.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := pageDescription(path)
	if got != "The real one-line summary." {
		t.Fatalf("unexpected description: %q", got)
	}
}

func TestCompactMarkdownDescription_TruncatesUTF8Safely(t *testing.T) {
	input := strings.Repeat("word ", 43) + "žluťoučký kůň"
	got := compactMarkdownDescription(input)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncated description, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated description is not valid UTF-8: %q", got)
	}
}

func TestCompactMarkdownDescription_ToleratesInvalidUTF8(t *testing.T) {
	input := string([]byte{0x80, 0x80, 0x80}) + strings.Repeat("word ", 60)
	got := compactMarkdownDescription(input)
	if !utf8.ValidString(got) {
		t.Fatalf("description is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncated description, got %q", got)
	}
}

func TestLLMSOutputHasGeneratedHeader(t *testing.T) {
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "index.md"), []byte("# Home\n\nIntro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mkdocsConfig{DocsDir: "docs"}
	if err := yaml.Unmarshal([]byte(`- Start: index.md`), &cfg.Nav); err != nil {
		t.Fatal(err)
	}
	rendered, err := renderLLMsFromConfig(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := genHeader + rendered
	if !strings.HasPrefix(body, genHeader) || !strings.Contains(body, "# Vornik") {
		t.Fatalf("llms output should carry generated header")
	}
}

func renderSectionSketch(sections []llmsSection) string {
	var b strings.Builder
	for _, s := range sections {
		for _, p := range s.Pages {
			b.WriteString(s.Title)
			b.WriteString(":")
			b.WriteString(p.Path)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestRenderConfig_OnlyDocTaggedFields(t *testing.T) {
	type Nested struct {
		Enabled bool   `yaml:"enabled" doc:"Turn the thing on."`
		Secret  string `yaml:"secret"` // no doc tag => excluded
	}
	type Cfg struct {
		API      Nested `yaml:"api"`
		Internal string `yaml:"internal_only"` // no doc tag => excluded
	}

	out := renderConfig(reflect.TypeOf(Cfg{}))
	if !strings.Contains(out, "`api.enabled`") {
		t.Errorf("doc-tagged nested key must appear; got:\n%s", out)
	}
	if !strings.Contains(out, "Turn the thing on.") {
		t.Errorf("doc description must appear; got:\n%s", out)
	}
	if strings.Contains(out, "api.secret") || strings.Contains(out, "internal_only") {
		t.Errorf("untagged fields must be excluded (deny-by-default); got:\n%s", out)
	}
	if !strings.Contains(out, "## api") {
		t.Errorf("expected section grouping by top-level key; got:\n%s", out)
	}
}
