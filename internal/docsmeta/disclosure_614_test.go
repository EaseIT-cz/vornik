package docsmeta

import (
	"strings"
	"testing"
)

// #614 C1/C2 (architect design §4.2): the shared pieces of the disclosure
// marks, the brief lint and the brief input. Before the change none existed.

func TestParseFrontmatter_DisclosureFields_614(t *testing.T) {
	md := []byte("---\ndisclosure: customer\ndisclosureExclude:\n    - \"## 5. Residual risks\"\nsources:\n    - path: a.go\n---\n# T\n")
	fm, had, err := ParseFrontmatter(md)
	if err != nil || !had {
		t.Fatalf("parse: had=%v err=%v", had, err)
	}
	if fm.Disclosure != "customer" || len(fm.DisclosureExclude) != 1 || fm.DisclosureExclude[0] != "## 5. Residual risks" {
		t.Fatalf("fields did not round-trip: %+v", fm)
	}
}

const sectioned = "# Design\n\nIntro.\n\n## 1. How it works\n\nThe flow.\n\n### 1.1 Detail\n\nMore.\n\n## 5. Residual risks\n\nSECRET gap.\n\n### 5.1 Sub-risk\n\nSECRET too.\n\n```md\n## not a heading inside a fence\n```\n\n## 6. Tests\n\nTests.\n"

func TestHeadings_SkipsFencedLines_614(t *testing.T) {
	got := strings.Join(Headings([]byte(sectioned)), "|")
	want := "# Design|## 1. How it works|### 1.1 Detail|## 5. Residual risks|### 5.1 Sub-risk|## 6. Tests"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestStripSections_DropsTheHeadingAndItsSubtree_614(t *testing.T) {
	out, missing := StripSections([]byte(sectioned), []string{"## 5. Residual risks", "## 9. Absent"})
	s := string(out)
	if strings.Contains(s, "SECRET") || strings.Contains(s, "Residual risks") || strings.Contains(s, "not a heading inside") {
		t.Fatalf("the excluded section survived:\n%s", s)
	}
	for _, keep := range []string{"## 1. How it works", "### 1.1 Detail", "## 6. Tests", "Tests."} {
		if !strings.Contains(s, keep) {
			t.Fatalf("%q was dropped:\n%s", keep, s)
		}
	}
	if len(missing) != 1 || missing[0] != "## 9. Absent" {
		t.Fatalf("missing headings must be reported: %v", missing)
	}
}

func TestSharedRun_614(t *testing.T) {
	src := "The executor writes the step result through recordStepResult before it routes on the outcome, so a later agent can read it from the outputs map of the run."
	words := strings.Fields(src)
	copied := strings.Join(words[:25], " ")
	if !SharedRun("A brief that says: "+copied+" and more.", src, 25) {
		t.Fatal("a copied 25-word run must be found")
	}
	if SharedRun(strings.Join(words[:24], " "), src, 25) {
		t.Fatal("a 24-word run must pass")
	}
	// Case and punctuation do not hide a copy; identifiers stay whole.
	shouted := strings.ToUpper(strings.ReplaceAll(copied, " ", ",  "))
	if !SharedRun(shouted, src, 25) {
		t.Fatal("case and punctuation must not hide a copy")
	}
	// A full stop added or removed does not hide a copy.
	if !SharedRun(strings.ReplaceAll(copied, " ", ". "), src, 25) {
		t.Fatal("full stops must not hide a copy")
	}
	if SharedRun("Results are recorded before routing, so the next agent sees them.", src, 25) {
		t.Fatal("a paraphrase must pass")
	}
}

// BriefInput is what the brief generator is shown: a customer design without
// its excluded sections and its front matter. An unmarked or never design is
// refused, and so is an exclusion naming a heading the design does not have
// (a renamed section would otherwise leak).
func TestBriefInput_614(t *testing.T) {
	fm := func(lines string) string { return "---\n" + lines + "---\n" }
	for _, tc := range []struct {
		name, md, wantErr string
	}{
		{"unmarked", sectioned, "not marked disclosure: customer"},
		{"never", fm("disclosure: never\n") + sectioned, "not marked disclosure: customer"},
		{"stale exclusion", fm("disclosure: customer\ndisclosureExclude:\n    - \"## 5. Residual\"\n") + sectioned, "## 5. Residual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BriefInput([]byte(tc.md)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
	out, err := BriefInput([]byte(fm("disclosure: customer\ndisclosureExclude:\n    - \"## 5. Residual risks\"\nsources:\n    - path: x.go\n") + sectioned))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "SECRET") || strings.Contains(s, "disclosure:") || strings.Contains(s, "x.go") {
		t.Fatalf("the input leaked an excluded section or the front matter:\n%s", s)
	}
	if !strings.Contains(s, "## 1. How it works") {
		t.Fatalf("the input lost the design:\n%s", s)
	}
}

// review-20261104-575b: the shapes that hid a heading from every control.
func TestDisclosureParsing_ReviewCases_614(t *testing.T) {
	// CRLF and a tab after the hashes are still headings, matched trimmed.
	crlf := "# T\r\n\r\n## 5. Residual risks\r\n\r\nSECRET\r\n\r\n##\t6. Tests\r\nok\r\n"
	if got := strings.Join(Headings([]byte(crlf)), "|"); got != "# T|## 5. Residual risks|##\t6. Tests" {
		t.Fatalf("headings: %q", got)
	}
	out, missing := StripSections([]byte(crlf), []string{"## 5. Residual risks"})
	if strings.Contains(string(out), "SECRET") || len(missing) != 0 {
		t.Fatalf("CRLF section not stripped (missing %v):\n%s", missing, out)
	}
	// A child excluded beside its parent is found, so lint and input agree.
	_, missing = StripSections([]byte(sectioned), []string{"## 5. Residual risks", "### 5.1 Sub-risk"})
	if len(missing) != 0 {
		t.Fatalf("a nested exclusion was reported missing: %v", missing)
	}
	// Shapes a customer design may not have: a setext heading, an unclosed fence.
	for name, body := range map[string]string{
		"setext =":       "# T\n\nResidual risks\n==============\n\nSECRET\n",
		"setext -":       "# T\n\nResidual risks\n--------------\n\nSECRET\n",
		"unclosed fence": "# T\n\n```\n## Residual risks\n\nSECRET\n",
	} {
		if err := CheckDisclosureShape([]byte(body)); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
		md := "---\ndisclosure: customer\n---\n" + body
		if _, err := BriefInput([]byte(md)); err == nil {
			t.Errorf("%s: BriefInput must refuse it", name)
		}
	}
	// A horizontal rule after a blank line is not a setext heading.
	if err := CheckDisclosureShape([]byte("# T\n\ntext\n\n---\n\nmore\n")); err != nil {
		t.Errorf("a rule after a blank line must pass: %v", err)
	}
}

// A design whose sources: list is plain strings (an older shape) failed
// ParseFrontmatter as a whole, so its disclosure mark was invisible to the
// lint and BriefInput refused it (found applying the first marking batch,
// 2026-11-04). The disclosure fields parse on their own.
func TestParseDisclosure_IgnoresAnUnrelatedSourcesShape_614(t *testing.T) {
	md := []byte("---\ndisclosure: customer\ndisclosureExclude:\n    - \"## 5. Residual risks\"\nsources:\n    - internal/api/x.go\n---\n# T\n\n## 5. Residual risks\n\nSECRET\n\n## 6. Tests\n")
	mark, excl, err := ParseDisclosure(md)
	if err != nil || mark != DisclosureCustomer || len(excl) != 1 {
		t.Fatalf("got %q %v %v", mark, excl, err)
	}
	out, err := BriefInput(md)
	if err != nil || strings.Contains(string(out), "SECRET") {
		t.Fatalf("BriefInput: %v\n%s", err, out)
	}
}
