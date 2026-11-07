package docsmeta

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// The shared pieces of the A2A architect's disclosure controls (#614 C1,
// C2; architect design §4.2): heading parsing, section stripping, and the
// word-run comparison. The brief lint (cmd/lint-lld-contracts) and the brief
// input (docs-gen brief-input) both use them, so the section the lint
// accepts as excluded is exactly the one the input drops.

// Disclosure values of Frontmatter.Disclosure.
const (
	DisclosureCustomer = "customer"
	DisclosureNever    = "never"
)

// headingLevel returns the ATX heading level of line (1-6), or 0.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || (line[n] != ' ' && line[n] != '\t') {
		return 0
	}
	return n
}

// splitLines splits text into lines without their line endings (LF or CRLF).
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// isFence reports whether line opens or closes a fenced code block.
func isFence(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// Headings returns the body's heading lines, trimmed of trailing space, in
// order. Lines inside fenced code blocks are not headings.
func Headings(body []byte) []string {
	var out []string
	inFence := false
	for _, line := range splitLines(string(body)) {
		if isFence(line) {
			inFence = !inFence
			continue
		}
		if !inFence && headingLevel(line) > 0 {
			out = append(out, strings.TrimRight(line, " \t"))
		}
	}
	return out
}

// StripSections drops each named heading (matched by exact trimmed line) with
// everything up to the next heading of the same or a higher level. It returns
// the remaining text and the names it did not find.
func StripSections(body []byte, headings []string) ([]byte, []string) {
	want := map[string]bool{}
	for _, h := range headings {
		want[h] = true
	}
	found := map[string]bool{}
	var kept []string
	inFence, dropLevel := false, 0
	for _, line := range splitLines(string(body)) {
		if isFence(line) {
			inFence = !inFence
		} else if lvl := headingLevel(line); !inFence && lvl > 0 {
			if dropLevel > 0 && lvl <= dropLevel {
				dropLevel = 0
			}
			if t := strings.TrimRight(line, " \t"); want[t] {
				// Found even inside a dropped parent, so an exclusion the
				// lint accepts is never reported missing here (575b F3).
				found[t] = true
				if dropLevel == 0 {
					dropLevel = lvl
				}
			}
		}
		if dropLevel == 0 {
			kept = append(kept, line)
		}
	}
	var missing []string
	for _, h := range headings {
		if !found[h] {
			missing = append(missing, h)
		}
	}
	return []byte(strings.Join(kept, "\n")), missing
}

// runWords lowercases text and splits it into words of letters, digits and
// the identifier characters _ . - (so recordStepResult or a2a.answer stays
// one word); everything else separates.
func runWords(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' && r != '-'
	})
	out := fields[:0]
	for _, w := range fields {
		// A sentence's full stop is not part of its last word.
		if w = strings.Trim(w, ".-"); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// SharedRun reports whether a repeats, word for word, a run of n or more
// consecutive words of b (lowercased, punctuation ignored). It is the
// verbatim backstop of the brief lint, not a paraphrase check.
func SharedRun(a, b string, n int) bool {
	bw := runWords(b)
	if n <= 0 || len(bw) < n {
		return false
	}
	runs := make(map[string]bool, len(bw)-n+1)
	for i := 0; i+n <= len(bw); i++ {
		runs[strings.Join(bw[i:i+n], " ")] = true
	}
	aw := runWords(a)
	for i := 0; i+n <= len(aw); i++ {
		if runs[strings.Join(aw[i:i+n], " ")] {
			return true
		}
	}
	return false
}

// BriefInput returns what an architect brief's author may be shown of a
// design (#614 C1): its body without the front matter and without each
// disclosureExclude section. It refuses a design not marked customer, and an
// exclusion that names no heading of the design.
func BriefInput(raw []byte) ([]byte, error) {
	mark, exclude, err := ParseDisclosure(raw)
	if err != nil {
		return nil, fmt.Errorf("front matter: %w", err)
	}
	if mark != DisclosureCustomer {
		return nil, fmt.Errorf("the design is not marked disclosure: customer (got %q); only a customer design may be a brief's source", mark)
	}
	_, body, _ := SplitFrontmatter(raw)
	if err := CheckDisclosureShape(body); err != nil {
		return nil, err
	}
	out, missing := StripSections(body, exclude)
	if len(missing) > 0 {
		return nil, fmt.Errorf("disclosureExclude names headings the design does not have: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// setextUnderline matches a setext heading's underline.
var setextUnderline = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)

// CheckDisclosureShape refuses the two shapes that hide a heading from
// Headings and StripSections (review-20261104-575b F1, F4, F5): a setext
// heading (a text line underlined with = or -), and a fence never closed.
// A customer design uses # headings; a rule after a blank line is fine.
func CheckDisclosureShape(body []byte) error {
	inFence, prev := false, ""
	for i, line := range splitLines(string(body)) {
		if isFence(line) {
			inFence = !inFence
			prev = ""
			continue
		}
		if !inFence && setextUnderline.MatchString(line) && strings.TrimSpace(prev) != "" && headingLevel(prev) == 0 {
			return fmt.Errorf("line %d underlines %q as a setext heading: write it as a # heading so the disclosure controls can see it", i+1, strings.TrimSpace(prev))
		}
		prev = line
	}
	if inFence {
		return fmt.Errorf("a fenced code block is never closed, which hides every heading after it")
	}
	return nil
}

// ParseDisclosure reads only the disclosure fields of a document's front
// matter, so a sources: list in an older shape (plain strings) cannot hide a
// mark: ParseFrontmatter fails on that shape as a whole (#614, found applying
// the first marking batch). No front matter means no mark.
func ParseDisclosure(raw []byte) (mark string, exclude []string, err error) {
	fmBytes, _, had := SplitFrontmatter(raw)
	if !had {
		return "", nil, nil
	}
	var d struct {
		Disclosure        string   `yaml:"disclosure"`
		DisclosureExclude []string `yaml:"disclosureExclude"`
	}
	if err := yaml.Unmarshal(fmBytes, &d); err != nil {
		return "", nil, err
	}
	return d.Disclosure, d.DisclosureExclude, nil
}
