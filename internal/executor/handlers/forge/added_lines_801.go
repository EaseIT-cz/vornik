package forge

// GitHub #801 C (github-app-channel design, "A review good by default"):
// the added lines of a unified diff, by new-file path and line number, so a
// finding can be checked against the diff it claims to be about.

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// addedLines maps a new-file path to the line numbers its hunks add.
type addedLines map[string]map[int]bool

// has reports whether file:line is an added line.
func (a addedLines) has(file string, line int) bool {
	return a[strings.TrimPrefix(file, "./")][line]
}

// parseAddedLines reads the "+++ b/<path>" headers and each hunk's
// "@@ -a,b +c,d @@". Inside a hunk it counts the old and new lines the
// header announces, so an added line that looks like a header ("+++ b/x",
// "@@ ...", in a patch fixture) is content, not metadata. A header that
// does not parse is counted in anomalies and its hunk skipped, never
// guessed.
func parseAddedLines(diff []byte) (out addedLines, anomalies int) {
	out = addedLines{}
	var file string
	line, oldLeft, newLeft := 0, 0, 0
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		t := sc.Text()
		if oldLeft > 0 || newLeft > 0 { // inside a hunk: content
			switch {
			case strings.HasPrefix(t, "+"):
				if file != "" {
					if out[file] == nil {
						out[file] = map[int]bool{}
					}
					out[file][line] = true
				}
				line++
				newLeft--
			case strings.HasPrefix(t, "-"):
				oldLeft--
			case strings.HasPrefix(t, `\`): // "\ No newline at end of file"
			default:
				line++
				oldLeft--
				newLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "diff --git "):
			file = ""
		case strings.HasPrefix(t, "+++ "):
			p := strings.TrimPrefix(t, "+++ ")
			if p == "/dev/null" {
				file = ""
				continue
			}
			file = strings.TrimPrefix(p, "b/")
			if i := strings.IndexByte(file, '\t'); i >= 0 { // a timestamp after a tab
				file = file[:i]
			}
		case strings.HasPrefix(t, "@@ "):
			o, n, start, ok := hunkHeader(t)
			if !ok {
				anomalies++
				continue
			}
			oldLeft, newLeft, line = o, n, start
		}
	}
	return out, anomalies
}

// hunkHeader parses "@@ -a[,b] +c[,d] @@": the old and new line counts
// (1 when omitted) and the new-side start.
func hunkHeader(h string) (oldN, newN, start int, ok bool) {
	f := strings.Fields(h)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return 0, 0, 0, false
	}
	count := func(s string) (first, n int, ok bool) {
		a, b, has := strings.Cut(s, ",")
		first, err := strconv.Atoi(a)
		if err != nil {
			return 0, 0, false
		}
		n = 1
		if has {
			if n, err = strconv.Atoi(b); err != nil {
				return 0, 0, false
			}
		}
		return first, n, true
	}
	_, oldN, ok1 := count(f[1][1:])
	start, newN, ok2 := count(f[2][1:])
	return oldN, newN, start, ok1 && ok2
}
