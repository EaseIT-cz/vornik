package docrender

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// docrender ships inside the agent image (via internal/agentloop) and in the
// daemon (via internal/dispatcher): it must stay a stdlib-only leaf.
func TestImportsAreStdlibOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); strings.Contains(p, ".") {
				t.Errorf("%s imports %s: docrender is a stdlib-only leaf", e.Name(), p)
			}
		}
	}
}

// audit 2026-10-12 (#339): render_document's PDF path used WeasyPrint's
// default fetcher (file: read) and the docx path pandoc's media fetch. The
// sandbox PDF program must use the shared reader, filter and a deny-all
// fetcher, and never hand pandoc a PDF engine.
func TestSandboxPDFScriptUsesTheSharedSanitizer(t *testing.T) {
	for _, want := range []string{
		`"--from=` + PandocFrom + `"`, `"--lua-filter=` + SandboxFilter + `"`, `"--metadata-file=` + SandboxMeta + `"`,
		`url_fetcher=deny_url`, `raise ValueError("external resources are disabled")`, `"` + SandboxPDFOut + `"`,
	} {
		if !strings.Contains(SandboxPDFScript, want) {
			t.Errorf("SandboxPDFScript lacks %s", want)
		}
	}
	if strings.Contains(SandboxPDFScript, "pdf-engine") {
		t.Error("SandboxPDFScript must not use a pandoc PDF engine: its fetcher is WeasyPrint's default")
	}
	// One fetcher for both programs (review-20261012-b33c F1).
	for name, script := range map[string]string{"WeasyPrintScript": WeasyPrintScript, "SandboxPDFScript": SandboxPDFScript} {
		if !strings.Contains(script, WeasyPrintPrelude) || !strings.Contains(script, "url_fetcher=deny_url") {
			t.Errorf("%s does not use the shared deny-all prelude", name)
		}
		if strings.Count(script, "def deny_url") != 1 {
			t.Errorf("%s defines its own fetcher", name)
		}
	}
	if PandocFrom != "gfm-yaml_metadata_block" {
		t.Errorf("PandocFrom = %q: front matter must stay off", PandocFrom)
	}
	args := strings.Join(SandboxPandocArgs(), " ")
	if args != "/in/input.md --from=gfm-yaml_metadata_block --lua-filter=/in/sanitize.lua --metadata-file=/in/meta.yaml" {
		t.Errorf("SandboxPandocArgs = %s", args)
	}
}
