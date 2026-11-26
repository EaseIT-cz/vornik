package forgeci

// CI artifact content (CI outcomes design §19, GitHub #876 and #872).
//
// GitHub serves every artifact as a ZIP. The ingest unpacks it in memory
// under one uncompressed budget, masks each entry and each label with the
// secret detector before any other cut, renders SARIF from a field
// whitelist, and stores text. Nothing is written to disk, so an entry name is
// only a label.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"vornik.io/vornik/internal/secrets"
)

const (
	// maxZipEntries is how many entries one artifact contributes.
	maxZipEntries = 64
	// maxSARIFResults is how many results one SARIF file shows.
	maxSARIFResults = 200
	// maxMessageRunes and maxLabelRunes cut a SARIF message and a label.
	maxMessageRunes = 300
	maxLabelRunes   = 120

	withheldNoDetector = "[content withheld: artifact content needs secrets.enabled]"
	withheldNoProof    = "[content withheld: the secret mask could not be proven]"
)

// zipEntry is one file read from the archive.
type zipEntry struct {
	name string
	body []byte
	size int // the bytes read, before any line cut
}

// readZipEntries unpacks data. Entries are read in archive order through one
// budget of uncompressed bytes; the entry the budget cuts keeps only its
// complete lines, and no later entry is read. Directories are skipped.
// Entries past maxEntries, after the budget ran out, or emptied by the cut
// are counted in notRead, not listed. The budget covers entry data; the
// central directory is bounded by the download's own cap.
func readZipEntries(data []byte, budget int64, maxEntries int) (entries []zipEntry, notRead int, truncated bool, err error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, false, err
	}
	remaining := budget
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
			continue
		}
		if len(entries) >= maxEntries || truncated {
			notRead++
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, false, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, remaining+1))
		_ = rc.Close()
		if err != nil {
			return nil, 0, false, err
		}
		size := len(body)
		if int64(len(body)) > remaining {
			body = body[:remaining]
			// Keep complete lines only: a value cut in half on the last line
			// would no longer match the detector.
			body = body[:bytes.LastIndexByte(body, '\n')+1]
			truncated = true
		}
		remaining -= int64(len(body))
		if truncated && len(body) == 0 {
			notRead++ // nothing of it is shown, so it gets no header
			continue
		}
		entries = append(entries, zipEntry{name: f.Name, body: body, size: size})
	}
	return entries, notRead, truncated, nil
}

// isText reports whether b is UTF-8 with no NUL byte.
func isText(b []byte) bool {
	return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

// fold replaces control characters (newlines included) with spaces and cuts
// s to n runes, so a label or a field stays on its own line.
func fold(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// escapeLookalikes prefixes a text line that could pose as a header, a SARIF
// block or a SARIF result line, so pushed text cannot imitate the ingest's
// own structure.
func escapeLookalikes(text string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "=== ") || strings.HasPrefix(l, "SARIF ") || strings.HasPrefix(l, "- [") {
			lines[i] = "| " + l
		}
	}
	return strings.Join(lines, "")
}

func isSARIFName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".sarif") || strings.HasSuffix(n, ".sarif.json")
}

// sarifFile is the whitelist of SARIF 2.1.0 fields the ingest reads; a field
// not named here (a snippet, a fingerprint) is never decoded.
type sarifFile struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name string `json:"name"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID  string `json:"ruleId"`
			Level   string `json:"level"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
			Locations []struct {
				PhysicalLocation struct {
					ArtifactLocation struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
	} `json:"runs"`
}

// renderSARIF turns one masked SARIF file into lines (design §19.3). label is
// the entry's folded name; rawValid says whether the file parsed before it was
// masked, so a file the mask broke is named as such. Each decoded field is
// masked again before it is folded: a JSON escape can hide a value from the
// mask of the raw file, and the fold's cut must not split one.
func renderSARIF(label string, masked []byte, rawValid bool, mask func([]byte) ([]byte, error)) (string, error) {
	var doc sarifFile
	if err := json.Unmarshal(masked, &doc); err != nil || doc.Version != "2.1.0" {
		if rawValid && err != nil {
			return fmt.Sprintf("SARIF %s: not SARIF 2.1.0 after masking\n", label), nil
		}
		return fmt.Sprintf("SARIF %s: not SARIF 2.1.0\n", label), nil
	}
	var maskErr error
	field := func(v string, n int) string {
		m, err := mask([]byte(v))
		if err != nil {
			maskErr = err
			return ""
		}
		return fold(string(m), n)
	}
	var tools []string
	total := 0
	var lines strings.Builder
	shown := 0
	for _, run := range doc.Runs {
		if name := field(run.Tool.Driver.Name, maxLabelRunes); name != "" {
			tools = append(tools, name)
		}
		for _, r := range run.Results {
			total++
			if shown >= maxSARIFResults {
				continue
			}
			shown++
			level := r.Level
			if level == "" {
				level = "warning" // SARIF 2.1.0 §3.27.10: absent means warning
			}
			where := "?"
			if len(r.Locations) > 0 {
				loc := r.Locations[0].PhysicalLocation
				where = sarifWhere(field(loc.ArtifactLocation.URI, maxLabelRunes), loc.Region.StartLine)
			}
			fmt.Fprintf(&lines, "- [%s] %s at %s: %s\n", field(level, 16), field(r.RuleID, maxLabelRunes), where, field(r.Message.Text, maxMessageRunes))
		}
	}
	tool := strings.Join(tools, ", ")
	if tool == "" {
		tool = "a scanner"
	}
	if maskErr != nil {
		return "", maskErr
	}
	return fmt.Sprintf("SARIF %s: %s reported %d result(s); %d shown\n", label, tool, total, shown) + lines.String(), nil
}

// sarifWhere is a result's place: its file, or "?", and its line when known.
func sarifWhere(uri string, line int) string {
	if uri == "" {
		uri = "?"
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", uri, line)
	}
	return uri
}

// artifactText is the stored text for one downloaded artifact, whether it is
// incomplete, and, when the content is withheld, why. A nil detector, or a
// mask the rescan disproves, withholds the content with a fixed note (design
// §19.2). An archive that does not unpack, including a decompress error in
// any entry, is an error: the whole artifact is then forfeit.
func artifactText(data []byte, budget int64, d secrets.Detector) (text string, partial bool, withheldBy error, err error) {
	if d == nil {
		return withheldNoDetector, false, secrets.ErrNoDetector, nil
	}
	entries, notRead, truncated, err := readZipEntries(data, budget, maxZipEntries)
	if err != nil {
		return "", false, nil, err
	}
	mask := func(b []byte) ([]byte, error) {
		m, _, err := secrets.MaskProven(d, b, nil)
		return m, err
	}
	var parts []string
	for _, e := range entries {
		// Masking is the first cut: the whole entry and its label are masked
		// before any render, label rule or cap touches them.
		name, err := mask([]byte(e.name))
		if err != nil {
			return withheld(err), false, err, nil
		}
		label := fold(string(name), maxLabelRunes)
		if !isText(e.body) {
			parts = append(parts, fmt.Sprintf("=== %s (%d bytes): binary, not shown ===\n", label, e.size))
			continue
		}
		body, err := mask(e.body)
		if err != nil {
			return withheld(err), false, err, nil
		}
		if isSARIFName(e.name) {
			rendered, err := renderSARIF(label, body, json.Valid(e.body), mask)
			if err != nil {
				return withheld(err), false, err, nil
			}
			parts = append(parts, rendered)
			continue
		}
		text := escapeLookalikes(string(body))
		if len(entries) == 1 && notRead == 0 {
			parts = append(parts, text)
			continue
		}
		parts = append(parts, fmt.Sprintf("=== %s (%d bytes) ===\n%s", label, e.size, text))
	}
	if notRead > 0 {
		parts = append(parts, fmt.Sprintf("=== %d more entries not read ===\n", notRead))
		truncated = true
	}
	// The proof: the final text, as it will be stored, rescans clean.
	final, err := mask([]byte(strings.Join(parts, "")))
	if err != nil {
		return withheld(err), false, err, nil
	}
	return string(final), truncated, nil, nil
}

func withheld(err error) string {
	if errors.Is(err, secrets.ErrNoDetector) {
		return withheldNoDetector
	}
	return withheldNoProof
}

// capText trims s to limit bytes, backing off to a rune boundary and to
// before an unfinished redaction marker, and reports whether it cut.
func capText(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	head := s[:cut]
	if i := strings.LastIndex(head, "[REDACTED:"); i >= 0 && !strings.Contains(head[i:], "]") {
		head = head[:i]
	}
	return head, true
}
