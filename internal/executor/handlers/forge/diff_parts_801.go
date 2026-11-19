package forge

// GitHub #801 G (github-app-channel design, "A review good by default"):
// order a diff's files code first and keep whole files within a budget; the
// rest are named, for the reviewers to read from the checkout.

import (
	"bytes"
	"path"
	"sort"
	"strconv"
	"strings"
)

// diffInlineBudget is how much of the masked diff the next agent gets
// inline, leaving the 256 KiB chain room for the readability and CI parts.
const diffInlineBudget = 192 << 10

type diffFile struct {
	path string
	n    int // 1-based position in the diff, to name a file without a path
	text []byte
}

// name is the file's path, or "<file N without a path>" (GitHub #814 F4: an
// unnamed over-budget file was dropped from not_in_diff and so from the
// review's not-reviewed line).
func (f diffFile) name() string {
	if f.path != "" {
		return f.path
	}
	return "<file " + strconv.Itoa(f.n) + " without a path>"
}

// splitDiffFiles cuts a unified diff at each "diff --git" line.
func splitDiffFiles(diff []byte) []diffFile {
	var out []diffFile
	for _, part := range bytes.SplitAfter(diff, []byte("\n")) {
		if len(part) == 0 {
			continue
		}
		if bytes.HasPrefix(part, []byte("diff --git ")) || len(out) == 0 {
			out = append(out, diffFile{path: diffGitPath(part), n: len(out) + 1})
		}
		f := &out[len(out)-1]
		f.text = append(f.text, part...)
		// #814 F5: a header git could not split falls back to the +++ line.
		if f.path == "" && bytes.HasPrefix(part, []byte("+++ ")) {
			f.path = bPath(strings.TrimSpace(string(part[4:])))
		}
	}
	return out
}

// diffGitPath is b/<path> of a "diff --git a/x b/y" line, quoted or not
// ("" otherwise).
func diffGitPath(line []byte) string {
	l := strings.TrimSpace(string(line))
	if !strings.HasPrefix(l, "diff --git ") {
		return ""
	}
	if i := strings.LastIndex(l, ` "b/`); i >= 0 {
		return bPath(l[i+1:])
	}
	if i := strings.LastIndex(l, " b/"); i >= 0 {
		return l[i+3:]
	}
	return ""
}

// bPath is the path of a "b/<path>" or "\"b/<path>\"" token ("" otherwise).
func bPath(tok string) string {
	if q, err := strconv.Unquote(tok); err == nil {
		tok = q
	}
	p, ok := strings.CutPrefix(tok, "b/")
	if !ok {
		return ""
	}
	return p
}

// fileRank orders code first, then tests, docs, and generated or lock files.
func fileRank(p string) int {
	base := path.Base(p)
	switch {
	case base == "go.sum" || base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "Cargo.lock" ||
		strings.Contains(base, ".min.") || strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/vendor/"):
		return 3
	case strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".txt") || strings.HasSuffix(base, ".rst") || strings.HasPrefix(p, "docs/"):
		return 2
	case strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasPrefix(p, "test/") || strings.Contains(p, "/test/"):
		return 1
	}
	return 0
}

// inlineDiff keeps whole files, code first, up to budget, and names the rest.
func inlineDiff(diff []byte, budget int) (kept []byte, notIn []string) {
	in, rest := splitInline(diff, budget)
	var b bytes.Buffer
	for _, f := range in {
		b.Write(f.text)
	}
	for _, f := range rest {
		notIn = append(notIn, f.name())
	}
	return b.Bytes(), notIn
}

// splitInline orders the files code first and splits them into those kept
// inline within budget and the rest, in order. inlineDiff and diffParts
// (#814) both use it, so the parts always follow not_in_diff.
func splitInline(diff []byte, budget int) (in, rest []diffFile) {
	files := splitDiffFiles(diff)
	sort.SliceStable(files, func(i, j int) bool { return fileRank(files[i].path) < fileRank(files[j].path) })
	size := 0
	for _, f := range files {
		if size+len(f.text) > budget {
			rest = append(rest, f)
			continue
		}
		size += len(f.text)
		in = append(in, f)
	}
	return in, rest
}
