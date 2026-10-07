// Command docs-gen generates the mechanically-derivable customer-docs pages
// (the config + CLI reference) from their source of truth, and stamps the
// provenance hashes on narrative pages. It is the anti-drift half of the docs
// pipeline: the reference pages are generator OUTPUT, never hand-edited, and
// CI fails if the committed page differs from a fresh generation.
//
// Usage:
//
//	docs-gen cli      # regenerate docs/public/reference/vornikctl.md
//	docs-gen config   # regenerate docs/public/reference/configuration.md
//	docs-gen llms     # regenerate docs/public/llms.txt from mkdocs.yml
//	docs-gen all      # all generated public docs + tool registry
//	docs-gen stamp    # re-anchor sources: hashes on all docs/public pages
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/cli"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/docsmeta"
)

const genHeader = "<!-- Generated from source — do not edit by hand. -->\n\n"

const (
	cliPage    = "docs/public/reference/vornikctl.md"
	configPage = "docs/public/reference/configuration.md"
	llmsPage   = "docs/public/llms.txt"
)

var markdownLinkRE = regexp.MustCompile(`!?\[(.*?)\]\([^)]*\)`)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: docs-gen <cli|config|editions|llms|tools|all|stamp>")
		os.Exit(2)
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	deny, err := docsmeta.LoadDenylist(filepath.Join(root, "scripts", "docs-ip-denylist.txt"))
	if err != nil {
		fatal(err)
	}
	switch os.Args[1] {
	case "cli":
		writePage(filepath.Join(root, cliPage), genHeader+renderCLI(cli.RootCmd(), loadCLIAllow(root)), deny)
	case "config":
		writePage(filepath.Join(root, configPage), genHeader+renderConfig(reflect.TypeOf(config.Config{})), deny)
	case "editions":
		writeEditions(root, deny)
	case "llms":
		writePage(filepath.Join(root, llmsPage), genHeader+renderLLMs(root), deny)
	case "tools":
		writeToolRegistry(root)
	case "all":
		writePage(filepath.Join(root, cliPage), genHeader+renderCLI(cli.RootCmd(), loadCLIAllow(root)), deny)
		writePage(filepath.Join(root, configPage), genHeader+renderConfig(reflect.TypeOf(config.Config{})), deny)
		writeEditions(root, deny)
		switch page, skipped, err := llmsForAll(root); {
		case err != nil:
			fatal(err)
		case skipped:
			// The Community export ships docs sources without the
			// Enterprise-owned publication config (export runbook,
			// amendment 2026-10-05). Said out loud, never silent.
			fmt.Fprintf(os.Stderr, "docs-gen: %s SKIPPED — no mkdocs.yml in this tree (publication config is Enterprise-owned)\n", llmsPage)
		default:
			writePage(filepath.Join(root, llmsPage), genHeader+page, deny)
		}
		writeToolRegistry(root)
	case "stamp":
		stamp(root, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "docs-gen: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

type mkdocsConfig struct {
	SiteName    string    `yaml:"site_name"`
	SiteURL     string    `yaml:"site_url"`
	DocsDir     string    `yaml:"docs_dir"`
	ExcludeDocs yaml.Node `yaml:"exclude_docs"`
	Nav         yaml.Node `yaml:"nav"`
}

type llmsSection struct {
	Title string
	Pages []llmsPageEntry
}

type llmsPageEntry struct {
	Title string
	Path  string
}

type excludeRules []string

// llmsForAll renders llms.txt for `docs-gen all`, or reports it skipped when
// the tree has no mkdocs.yml (the Community export removes it). `docs-gen
// llms` asked for explicitly still fails without it.
func llmsForAll(root string) (page string, skipped bool, err error) {
	path := filepath.Join(root, "mkdocs.yml")
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return "", true, nil
	}
	cfg, err := loadMkdocsConfig(path)
	if err != nil {
		return "", false, err
	}
	page, err = renderLLMsFromConfig(root, cfg)
	return page, false, err
}

func renderLLMs(root string) string {
	cfg, err := loadMkdocsConfig(filepath.Join(root, "mkdocs.yml"))
	if err != nil {
		fatal(err)
	}
	out, err := renderLLMsFromConfig(root, cfg)
	if err != nil {
		fatal(err)
	}
	return out
}

func renderLLMsFromConfig(root string, cfg mkdocsConfig) (string, error) {
	if cfg.DocsDir == "" {
		cfg.DocsDir = "docs"
	}
	siteURL := strings.TrimRight(cfg.SiteURL, "/")
	excluded := parseExcludeDocs(cfg.ExcludeDocs)

	sections, err := collectLLMSSections(cfg.Nav, excluded)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Vornik\n\n")
	b.WriteString("> Local-first orchestration daemon for teams of AI agents. Vornik runs asynchronous projects, swarms, workflows, memory, approvals, companion delegation, and operator tooling on infrastructure you control.\n\n")
	b.WriteString("Install: `curl -fsSL https://get.vornik.io | bash`  (Linux with rootless Podman recommended)\n\n")
	b.WriteString("Repo: https://github.com/EaseIT-cz/vornik\n")
	if siteURL != "" {
		fmt.Fprintf(&b, "Docs: %s\n", siteURL)
	}

	docsDir := filepath.Join(root, cfg.DocsDir)
	for _, section := range sections {
		if len(section.Pages) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n", section.Title)
		for _, page := range section.Pages {
			mdPath := filepath.Join(docsDir, filepath.FromSlash(page.Path))
			title, desc, err := readMarkdownSummary(mdPath)
			if err != nil {
				return "", fmt.Errorf("llms: %s: %w", page.Path, err)
			}
			if title == "" {
				title = page.Title
			}
			if desc == "" {
				desc = title
			}
			fmt.Fprintf(&b, "- [%s](%s): %s\n", title, publicDocsURL(siteURL, page.Path), desc)
		}
	}
	return b.String(), nil
}

func loadMkdocsConfig(path string) (mkdocsConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return mkdocsConfig{}, err
	}
	var cfg mkdocsConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return mkdocsConfig{}, err
	}
	if len(cfg.Nav.Content) == 0 {
		return mkdocsConfig{}, fmt.Errorf("mkdocs nav is empty")
	}
	return cfg, nil
}

func parseExcludeDocs(raw yaml.Node) excludeRules {
	var out excludeRules
	switch raw.Kind {
	case yaml.SequenceNode:
		for _, item := range raw.Content {
			if item.Kind == yaml.ScalarNode {
				addExcludeRule(&out, item.Value)
			}
		}
	case yaml.ScalarNode:
		for _, line := range strings.Split(raw.Value, "\n") {
			addExcludeRule(&out, line)
		}
	}
	return out
}

func addExcludeRule(out *excludeRules, raw string) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	*out = append(*out, line)
}

func (rules excludeRules) match(doc string) bool {
	for _, rule := range rules {
		if rule == doc {
			return true
		}
		if ok, _ := path.Match(rule, doc); ok {
			return true
		}
		if strings.HasSuffix(rule, "/**") && strings.HasPrefix(doc, strings.TrimSuffix(rule, "**")) {
			return true
		}
	}
	return false
}

func collectLLMSSections(nav yaml.Node, excluded excludeRules) ([]llmsSection, error) {
	if nav.Kind == yaml.DocumentNode && len(nav.Content) > 0 {
		nav = *nav.Content[0]
	}
	var sections []llmsSection
	for i := 0; i < len(nav.Content); i++ {
		item := nav.Content[i]
		if item.Kind == yaml.ScalarNode {
			if !excluded.match(item.Value) {
				sections = append(sections, llmsSection{
					Title: "Other",
					Pages: []llmsPageEntry{{Title: strings.TrimSuffix(filepath.Base(item.Value), ".md"), Path: item.Value}},
				})
			}
			continue
		}
		if item.Kind != yaml.MappingNode || len(item.Content) < 2 {
			return nil, fmt.Errorf("llms: malformed mkdocs nav entry at index %d", i)
		}
		title := item.Content[0].Value
		value := item.Content[1]
		if value.Kind == yaml.ScalarNode {
			if strings.TrimSpace(value.Value) == "" {
				return nil, fmt.Errorf("llms: mkdocs nav entry %q has no target", title)
			}
			if title == "Home" || excluded.match(value.Value) {
				continue
			}
			sections = append(sections, llmsSection{
				Title: title,
				Pages: []llmsPageEntry{{Title: title, Path: value.Value}},
			})
			continue
		}
		pages, err := collectLLMSPages(value, excluded)
		if err != nil {
			return nil, fmt.Errorf("llms: mkdocs nav section %q: %w", title, err)
		}
		if len(pages) > 0 {
			sections = append(sections, llmsSection{Title: title, Pages: pages})
		}
	}
	return sections, nil
}

//nolint:gocognit // Recursive mkdocs nav traversal is easier to audit in one switch.
func collectLLMSPages(node *yaml.Node, excluded excludeRules) ([]llmsPageEntry, error) {
	if node == nil {
		return nil, fmt.Errorf("empty nav node")
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return collectLLMSPages(node.Content[0], excluded)
	}
	var pages []llmsPageEntry
	switch node.Kind {
	case yaml.SequenceNode:
		for _, child := range node.Content {
			childPages, err := collectLLMSPages(child, excluded)
			if err != nil {
				return nil, err
			}
			pages = append(pages, childPages...)
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return nil, fmt.Errorf("mapping nav node has an unmatched key")
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			title := node.Content[i].Value
			value := node.Content[i+1]
			if value.Kind == yaml.ScalarNode {
				if strings.TrimSpace(value.Value) == "" {
					return nil, fmt.Errorf("entry %q has no target", title)
				}
				if !excluded.match(value.Value) {
					pages = append(pages, llmsPageEntry{Title: title, Path: value.Value})
				}
				continue
			}
			childPages, err := collectLLMSPages(value, excluded)
			if err != nil {
				return nil, fmt.Errorf("entry %q: %w", title, err)
			}
			pages = append(pages, childPages...)
		}
	case yaml.ScalarNode:
		if strings.TrimSpace(node.Value) == "" {
			return nil, fmt.Errorf("empty nav target")
		}
		if !excluded.match(node.Value) {
			pages = append(pages, llmsPageEntry{Title: strings.TrimSuffix(filepath.Base(node.Value), ".md"), Path: node.Value})
		}
	default:
		return nil, fmt.Errorf("unsupported nav node kind %d", node.Kind)
	}
	return pages, nil
}

func pageDescription(path string) string {
	_, desc, err := readMarkdownSummary(path)
	if err != nil {
		return ""
	}
	return desc
}

//nolint:gocognit // Markdown summary extraction is a small state machine kept together.
func readMarkdownSummary(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var para []string
	var title string
	inFrontMatter := false
	frontMatterChecked := false
	afterH1 := false
	skipAdmonition := false
	inCodeFence := false
	for scanner.Scan() {
		rawLine := scanner.Text()
		line := strings.TrimSpace(rawLine)
		if !frontMatterChecked {
			if line == "" {
				continue
			}
			frontMatterChecked = true
			if line == "---" {
				inFrontMatter = true
				continue
			}
		}
		if inFrontMatter {
			if line == "---" {
				inFrontMatter = false
			}
			continue
		}
		if strings.HasPrefix(line, "# ") {
			if title == "" {
				title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			}
			afterH1 = true
			continue
		}
		if strings.HasPrefix(line, "```") {
			inCodeFence = !inCodeFence
			continue
		}
		if inCodeFence {
			continue
		}
		if strings.HasPrefix(line, "!!! ") {
			skipAdmonition = true
			continue
		}
		if skipAdmonition {
			if strings.HasPrefix(rawLine, "    ") || line == "" {
				continue
			}
			skipAdmonition = false
		}
		if !afterH1 || line == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "- ") {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, line)
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return title, compactMarkdownDescription(strings.Join(para, " ")), nil
}

func compactMarkdownDescription(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	s = markdownLinkRE.ReplaceAllString(s, "$1")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 220 {
		return s
	}
	cutLimit := 220
	for cutLimit > 0 && !utf8.RuneStart(s[cutLimit]) {
		cutLimit--
	}
	if cutLimit == 0 && !utf8.RuneStart(s[0]) {
		cutLimit = 220
	}
	cut := strings.LastIndex(s[:cutLimit], " ")
	if cut < 120 {
		cut = cutLimit
	}
	return strings.TrimSpace(s[:cut]) + "..."
}

func publicDocsURL(siteURL, mdPath string) string {
	docPath := strings.TrimSuffix(mdPath, ".md")
	docPath = strings.TrimSuffix(docPath, "/index")
	if docPath == "index" {
		docPath = ""
	}
	if docPath != "" {
		docPath += "/"
	}
	if siteURL == "" {
		return "/" + docPath
	}
	return strings.TrimRight(siteURL, "/") + "/" + docPath
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "docs-gen: %v\n", err)
	os.Exit(1)
}

// writePage refuses to emit a page that trips the IP guard — a generated page
// is only as safe as its source, and cobra help / config comments can cite
// internal detail. A leak fails the build instead of shipping.
func writePage(path, content string, deny []string) {
	if hits := docsmeta.ForbiddenHits(content, deny); len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "docs-gen: refusing to write %s — IP markers in generated output:\n", path)
		for _, h := range hits {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
		fmt.Fprintln(os.Stderr, "fix the source (help text / config comment) or adjust the allowlist.")
		os.Exit(1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s\n", path)
}

// loadCLIAllow reads the customer-facing top-level command allowlist. Each
// non-empty, non-comment line is one top-level vornikctl command name. Missing
// file => empty allowlist => nothing emitted (deny-by-default).
func loadCLIAllow(root string) map[string]bool {
	allow := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(root, "scripts", "docs-cli-allowlist.txt"))
	if err != nil {
		return allow
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allow[line] = true
	}
	return allow
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// stamp re-anchors provenance hashes on docs that declare a sources: block.
//
// Bare `docs-gen stamp` keeps its historical behaviour: re-anchor every
// docs/public page. Those are generated narrative pages whose review model is
// the docs-gen pipeline itself.
//
// An LLD is different, and stamping one requires
// `--i-have-reviewed=https://docs.vornik.io`.
//
// WHY A MECHANICAL GUARD AND NOT A COMMENT (drift design §4.3, review finding 3):
// `sha256` means "hash at last review" — the anchor is a claim that a human read
// this document against that file. 269 of 273 LLDs are unanchored, so the
// tempting "fix" is a loop over the corpus. That would mint 269 false review
// claims and destroy the signal permanently and SILENTLY: every anchor would
// match, so the staleness check would pass forever while meaning nothing.
// Requiring one explicit flag per path makes the act deliberate and legible in
// shell history. It does not make lying impossible; it makes lying visible.
func stamp(root string, args []string) {
	reviewed, err := parseReviewedFlags(args)
	if err != nil {
		fatal(err)
	}

	if len(reviewed) == 0 {
		pub := filepath.Join(root, "docs", "public")
		_ = filepath.WalkDir(pub, func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			changed, rerr := docsmeta.Restamp(root, path)
			if rerr != nil {
				fatal(rerr)
			}
			if changed {
				rel, _ := filepath.Rel(root, path)
				fmt.Printf("stamped %s\n", rel)
			}
			return nil
		})
		return
	}

	for _, rel := range reviewed {
		stampReviewedDoc(root, rel)
	}
}

// stampReviewedDoc anchors one explicitly-reviewed document.
func stampReviewedDoc(root, rel string) {
	abs := filepath.Join(root, rel)
	raw, err := os.ReadFile(abs)
	if err != nil {
		fatal(fmt.Errorf("--i-have-reviewed=%s: %w", rel, err))
	}
	// An unbuilt design has nothing to have been reviewed AGAINST, so anchoring
	// it is meaningless by construction. This eliminates the most harmful class
	// of false anchor at the mechanical level.
	if class := statusClassOf(string(raw)); class == "pre-impl" {
		fatal(fmt.Errorf("refusing to anchor %s: its status reads pre-implementation, so there is "+
			"no shipped code for the anchor to attest a review against. Update the status when the "+
			"design lands, then anchor it", rel))
	}
	changed, err := docsmeta.Restamp(root, abs)
	if err != nil {
		fatal(err)
	}
	if changed {
		fmt.Printf("stamped %s (reviewed)\n", rel)
		return
	}
	fmt.Printf("%s already current\n", rel)
}

// parseReviewedFlags collects --i-have-reviewed=<path> occurrences. Deliberately
// accepts no glob, no directory, and no --all: one path per flag.
func parseReviewedFlags(args []string) ([]string, error) {
	const flag = "--i-have-reviewed="
	var out []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, flag):
			p := strings.TrimSpace(strings.TrimPrefix(a, flag))
			if p == "" {
				return nil, fmt.Errorf("--i-have-reviewed= needs a path")
			}
			if strings.ContainsAny(p, "*?[") {
				return nil, fmt.Errorf("--i-have-reviewed=%s: globs are refused — name one document "+
					"per flag. Anchoring asserts you read that document against its sources; a glob "+
					"cannot make that claim", p)
			}
			out = append(out, p)
		case a == "--all":
			return nil, fmt.Errorf("--all is refused: an anchor asserts a human review, so there is " +
				"no such thing as reviewing everything at once. Use one --i-have-reviewed=<path> per " +
				"document you have actually read")
		default:
			return nil, fmt.Errorf("stamp: unknown argument %q", a)
		}
	}
	return out, nil
}

// statusClassOf classifies a doc's `Status:` line the same way the drift linter
// does: leading token only, and "" for anything ambiguous. Kept deliberately
// simple and duplicated rather than shared, because docs-gen must not import the
// linter and the rule is four words long.
func statusClassOf(body string) string {
	m := reDocStatus.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(m[1]))
	s = strings.TrimLeft(s, "*_ ")
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '—' || r == '-' || r == '(' || r == ',' || r == ':' || r == ';'
	})
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "design", "draft", "proposed", "pending":
		return "pre-impl"
	case "implemented", "shipped", "delivered", "released", "complete":
		return "shipped"
	}
	return ""
}

var reDocStatus = regexp.MustCompile(`(?im)^\s*(?:\*\*)?status(?:\*\*)?\s*[::]\s*(.+)$`)

// ---------------------------------------------------------------------------
// CLI reference generation
// ---------------------------------------------------------------------------

// renderCLI walks a cobra command tree and emits a single Markdown reference.
// Deny-by-default: only top-level command groups named in allow are emitted
// (with all their descendants). This keeps internal/admin commands out of the
// customer docs even though they remain in `vornikctl --help` for operators.
func renderCLI(root *cobra.Command, allow map[string]bool) string {
	var b strings.Builder
	b.WriteString("# vornikctl CLI reference\n\n")
	if root.Long != "" {
		b.WriteString(root.Long + "\n\n")
	} else if root.Short != "" {
		b.WriteString(root.Short + "\n\n")
	}
	var emit func(c *cobra.Command)
	emit = func(c *cobra.Command) {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		b.WriteString("## " + c.CommandPath() + "\n\n")
		if c.Short != "" {
			b.WriteString(c.Short + "\n\n")
		}
		if c.Long != "" && c.Long != c.Short {
			b.WriteString(c.Long + "\n\n")
		}
		if c.Runnable() {
			b.WriteString("```\n" + strings.TrimSpace(c.UseLine()) + "\n```\n\n")
		}
		b.WriteString(renderFlags(c.LocalFlags()))
		if c.Example != "" {
			b.WriteString("Example:\n\n```\n" + strings.TrimSpace(c.Example) + "\n```\n\n")
		}
		children := append([]*cobra.Command(nil), c.Commands()...)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			emit(child)
		}
	}
	top := append([]*cobra.Command(nil), root.Commands()...)
	sort.Slice(top, func(i, j int) bool { return top[i].Name() < top[j].Name() })
	for _, c := range top {
		if allow[c.Name()] {
			emit(c)
		}
	}
	return b.String()
}

func renderFlags(fs *pflag.FlagSet) string {
	type row struct{ name, def, usage string }
	var rows []row
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		rows = append(rows, row{name, f.DefValue, f.Usage})
	})
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	b.WriteString("| Flag | Default | Description |\n|---|---|---|\n")
	for _, r := range rows {
		def := r.def
		if def != "" {
			def = "`" + def + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.name, def, escapePipes(r.usage))
	}
	b.WriteString("\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// config reference generation
// ---------------------------------------------------------------------------

type docRow struct{ key, typ, doc, env string }

// renderConfig reflects over the config struct type and emits a Markdown table
// of every field carrying a `doc:"..."` tag, grouped by top-level section.
// Deny-by-default: an untagged field is never published.
func renderConfig(t reflect.Type) string {
	rows := collectDocRows(t, "")
	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("vornik reads its configuration from `config.yaml`. The keys below are the customer-facing settings, by dotted YAML path. Where an environment variable overrides a key it is listed; an override wins over the file, and `vornikctl config show --provenance` shows which one supplied each value on the running daemon.\n\n")

	// Group by first path segment, preserving first-seen section order.
	var order []string
	groups := map[string][]docRow{}
	for _, r := range rows {
		sec := r.key
		if i := strings.IndexByte(r.key, '.'); i >= 0 {
			sec = r.key[:i]
		}
		if _, ok := groups[sec]; !ok {
			order = append(order, sec)
		}
		groups[sec] = append(groups[sec], r)
	}
	for _, sec := range order {
		b.WriteString("## " + sec + "\n\n")
		b.WriteString("| Key | Type | Description | Environment override |\n|---|---|---|---|\n")
		for _, r := range groups[sec] {
			env := "—"
			if r.env != "" {
				env = "`" + r.env + "`"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.key, r.typ, escapePipes(r.doc), env)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// collectDocRows walks every yaml-tagged leaf through config.WalkLeaves — the
// same enumeration the resolved-config dump uses, so a key cannot be
// documented and undumpable or the reverse — and keeps the doc-tagged ones.
func collectDocRows(t reflect.Type, _ string) []docRow {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var rows []docRow
	config.WalkLeaves(reflect.New(t).Elem(), func(key string, f reflect.StructField, _ reflect.Value) {
		// Skip top-level sections deliberately hidden from the PUBLIC config
		// reference. The trading feature is withheld from the published site
		// (mkdocs exclude_docs drops its feature/guide pages); the generated
		// config reference must not re-expose it via its keys. The keys still
		// work — they're just not advertised on docs.vornik.io.
		if publicDocExcludedSections[strings.SplitN(key, ".", 2)[0]] {
			return
		}
		doc := f.Tag.Get("doc")
		if doc == "" {
			return // deny-by-default — an untagged leaf is internal
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		rows = append(rows, docRow{key: key, typ: friendlyType(ft), doc: doc, env: config.EnvOverrideFor(key)})
	})
	return rows
}

var publicDocExcludedSections = map[string]bool{
	"trading": true,
}

func friendlyType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map:
		return "map"
	default:
		return t.Kind().String()
	}
}

func escapePipes(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
}
