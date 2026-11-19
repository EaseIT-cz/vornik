package forge

// GitHub #814 (github-app-channel design, "Reading a large diff in parts"):
// the files past the inline budget, packed into parts the review roles read
// with forge_diff_part. Computed from the masked diff alone, so the fetch
// that counts the parts and the reader that serves them agree.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/executor"
)

// diffPartBudget is the most one served part holds.
const diffPartBudget = 48 << 10

// servedParts are the parts fetch_diff counts and forge_diff_part serves.
func servedParts(masked []byte) [][]byte {
	return diffParts(masked, diffInlineBudget, diffPartBudget)
}

// diffParts returns the masked diff's parts: [0] is the inline text, the
// rest hold the files not inline, in not_in_diff's order. Whole files are
// packed up to part bytes; a file larger than that is cut into pieces.
func diffParts(masked []byte, inline, part int) [][]byte {
	in, rest := splitInline(masked, inline)
	var kept bytes.Buffer
	for _, f := range in {
		kept.Write(f.text)
	}
	parts := [][]byte{kept.Bytes()}
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, cur)
			cur = nil
		}
	}
	for _, f := range rest {
		if len(f.text) > part {
			flush()
			parts = append(parts, cutFile(f, part)...)
			continue
		}
		if len(cur)+len(f.text) > part {
			flush()
		}
		cur = append(cur, f.text...)
	}
	flush()
	return parts
}

// cutFile cuts one file's diff at line ends into pieces of at most part bytes
// (a single longer line is a piece of its own). Each piece repeats the file's
// header and a label; a piece that starts inside a hunk restarts it with the
// line numbers its first line has, so a finding there cites the right line.
func cutFile(f diffFile, part int) [][]byte {
	lines := bytes.SplitAfter(f.text, []byte("\n"))
	h := 0
	for h < len(lines) && !bytes.HasPrefix(lines[h], []byte("@@ ")) {
		h++
	}
	if h == len(lines) {
		return [][]byte{f.text} // no hunk to cut at (a binary or header-only diff): served whole
	}
	header := bytes.Join(lines[:h], nil)
	reserve := len(header) + len(f.name()) + 64 // the label and a restarted hunk header
	var bodies [][]byte
	var cur []byte
	inHunk := false
	oldLn, newLn := 0, 0
	for _, l := range lines[h:] {
		if len(l) == 0 {
			continue
		}
		// A "\ No newline at end of file" marker stays with the line it
		// belongs to (#814 review a38e L2).
		if len(cur) > 0 && l[0] != '\\' && reserve+len(cur)+len(l) > part {
			bodies = append(bodies, cur)
			cur = nil
			if inHunk && !bytes.HasPrefix(l, []byte("@@ ")) {
				cur = fmt.Appendf(nil, "@@ -%d +%d @@ (continued)\n", oldLn, newLn)
			}
		}
		cur = append(cur, l...)
		switch {
		case bytes.HasPrefix(l, []byte("@@ ")):
			oldLn, newLn = hunkStarts(l)
			inHunk = true
		case l[0] == ' ':
			oldLn++
			newLn++
		case l[0] == '-':
			oldLn++
		case l[0] == '+':
			newLn++
		}
	}
	if len(cur) > 0 {
		bodies = append(bodies, cur)
	}
	out := make([][]byte, len(bodies))
	for i, b := range bodies {
		out[i] = fmt.Appendf(append([]byte{}, header...), "[Vornik: %s, piece %d of %d]\n%s", f.name(), i+1, len(bodies), b)
	}
	return out
}

// hunkStarts reads "@@ -a[,b] +c[,d] @@" as the old and new start lines.
func hunkStarts(l []byte) (oldLn, newLn int) {
	fields := strings.Fields(string(l))
	if len(fields) > 3 {
		fields = fields[1:3] // the text after the closing @@ is the section, not numbers
	}
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		n, _, _ := strings.Cut(field[1:], ",")
		switch field[0] {
		case '-':
			oldLn, _ = strconv.Atoi(n)
		case '+':
			newLn, _ = strconv.Atoi(n)
		}
	}
	return oldLn, newLn
}

// notReviewed is the review's not-reviewed list (#814 §5): each not-inline
// file unless every part holding it is in some step's parts_read, plus the
// file the fetch cut partway, which is never reviewed whole. parts_read is
// the model's own report; it is not checked against anything else.
func notReviewed(diff string, notIn []string, cutFile string, steps map[string]json.RawMessage) []string {
	read := map[int]bool{}
	for _, raw := range steps {
		for _, n := range partsRead(raw) {
			read[n] = true
		}
	}
	holding := map[string][]int{}
	for i, p := range servedParts([]byte(diff)) {
		if i == 0 {
			continue
		}
		for _, f := range splitDiffFiles(p) {
			holding[f.name()] = append(holding[f.name()], i+1)
		}
	}
	var out []string
	for _, name := range notIn {
		whole := len(holding[name]) > 0
		for _, n := range holding[name] {
			whole = whole && read[n]
		}
		if !whole || name == cutFile {
			out = append(out, name)
		}
	}
	if cutFile != "" && !slices.Contains(out, cutFile) {
		out = append(out, cutFile)
	}
	return out
}

// partsRead is the parts_read list of an agent step's result: the last JSON
// object in its text, or the result itself.
func partsRead(raw json.RawMessage) []int {
	var pr struct {
		PartsRead []int `json:"parts_read"`
	}
	if json.Unmarshal(raw, &pr) == nil && len(pr.PartsRead) > 0 {
		return pr.PartsRead
	}
	var in reviewInput
	_ = json.Unmarshal(raw, &in)
	if obj := executor.ExtractLastJSONObject([]byte(firstNonEmpty(in.Message, in.Output, in.Result, in.Body))); obj != nil {
		_ = json.Unmarshal(obj, &pr)
	}
	return pr.PartsRead
}
