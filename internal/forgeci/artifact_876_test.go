package forgeci

// GitHub #876 and #872, CI outcomes design §19: a CI artifact arrives as a
// ZIP. The ingest unpacks it under one uncompressed budget, masks each entry
// and label with the secret detector before any other cut, renders SARIF
// from a field whitelist, and stores text, never the archive's bytes.
//
// Found at: review (#876, reading the path for #872). Before the fix the
// ingest stored string(zipBytes); TestArtifact_876_ZipTextIsStoredAsText is
// the test that fails on that code.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

const artifactName = "ci-report"

// testToken is secret-shaped for tokenDetector; the test never needs a real one.
const testToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

// tokenDetector finds ghp_ tokens of the full length, as the real detector's
// github_pat pattern does; a token cut short no longer matches.
type tokenDetector struct{}

var tokenRE = regexp.MustCompile(`ghp_[0-9A-Za-z]{38}`)

func (tokenDetector) Scan(text []byte) []secrets.Finding {
	var out []secrets.Finding
	for _, m := range tokenRE.FindAllIndex(text, -1) {
		out = append(out, secrets.Finding{Type: "github_pat", Start: m[0], End: m[1], Match: string(text[m[0]:m[1]])})
	}
	return out
}

// alwaysDetector finds something in any non-empty text, so no proof holds.
type alwaysDetector struct{}

func (alwaysDetector) Scan(text []byte) []secrets.Finding {
	if len(text) == 0 {
		return nil
	}
	return []secrets.Finding{{Type: "always", Start: 0, End: 1}}
}

// zipReader serves one artifact whose bytes are data.
type zipReader struct{ data []byte }

func (z zipReader) FetchCIRun(context.Context, string, int64) (forge.CIRun, error) {
	return forge.CIRun{}, nil
}
func (z zipReader) ListCIArtifacts(context.Context, string, int64) ([]forge.CIArtifact, error) {
	return []forge.CIArtifact{{ID: 7, Name: artifactName}}, nil
}
func (z zipReader) FetchCIArtifact(context.Context, string, int64, int64) ([]byte, error) {
	return z.data, nil
}
func (z zipReader) VerifyCIAccess(context.Context) error { return nil }

type entry struct{ name, body string }

func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// record runs one completed run through the ingest and returns the stored row.
func record(t *testing.T, data []byte, d secrets.Detector, tune func(*Config)) *persistence.ForgeCIOutcome {
	t.Helper()
	cfg := Config{ArtifactName: artifactName, MaxArtifactBytes: 1 << 20, MaxExcerptBytes: 64 << 10, Detector: d}
	if tune != nil {
		tune(&cfg)
	}
	g := New(&markingOutcomes{}, zipReader{data: data}, cfg, zerolog.Nop())
	out := g.Record(context.Background(), Run{ProjectID: "p", Repo: "o/r", RunID: 1, HeadSHA: "abc", Conclusion: "success"})
	if out == nil {
		t.Fatal("no outcome recorded")
	}
	return out
}

func TestArtifact_876_ZipTextIsStoredAsText(t *testing.T) {
	out := record(t, zipOf(t, entry{"plan.txt", "Plan: 1 to add, 0 to change.\n"}), tokenDetector{}, nil)
	if out.ArtifactExcerpt != "Plan: 1 to add, 0 to change.\n" {
		t.Fatalf("excerpt = %q, want the entry's text", out.ArtifactExcerpt)
	}
	if out.ArtifactTruncated {
		t.Fatal("a small artifact was marked truncated")
	}
}

func TestArtifact_876_SeveralEntriesHaveHeaders(t *testing.T) {
	out := record(t, zipOf(t, entry{"a.txt", "alpha\n"}, entry{"dir/", ""}, entry{"b.txt", "beta\n"}), tokenDetector{}, nil)
	for _, want := range []string{"=== a.txt (6 bytes) ===\nalpha\n", "=== b.txt (5 bytes) ===\nbeta\n"} {
		if !strings.Contains(out.ArtifactExcerpt, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, out.ArtifactExcerpt)
		}
	}
	if strings.Contains(out.ArtifactExcerpt, "dir/") {
		t.Error("a directory entry was listed")
	}
}

func TestArtifact_876_BinaryEntryIsNotedNotShown(t *testing.T) {
	out := record(t, zipOf(t, entry{"img.png", "\x89PNG\r\n\x1a\n\x00\x00binary"}, entry{"r.txt", "ok\n"}), tokenDetector{}, nil)
	if !strings.Contains(out.ArtifactExcerpt, "img.png") || !strings.Contains(out.ArtifactExcerpt, "binary, not shown") {
		t.Fatalf("binary entry not noted:\n%q", out.ArtifactExcerpt)
	}
	if strings.Contains(out.ArtifactExcerpt, "PNG") {
		t.Fatal("binary content was shown")
	}
}

func TestArtifact_876_BombStopsAtTheBudget(t *testing.T) {
	data := zipOf(t, entry{"bomb.txt", strings.Repeat("a\n", 5<<20)})
	if len(data) > 64<<10 {
		t.Fatalf("fixture is not a bomb: %d bytes compressed", len(data))
	}
	out := record(t, data, tokenDetector{}, func(c *Config) { c.MaxExcerptBytes = 1 << 20 })
	if !out.ArtifactTruncated {
		t.Fatal("a budget cut was not recorded")
	}
	if !strings.HasPrefix(out.ArtifactExcerpt, "a\n") || strings.Contains(out.ArtifactExcerpt, "PK\x03\x04") {
		t.Fatalf("not the uncompressed text: %q", out.ArtifactExcerpt[:min(40, len(out.ArtifactExcerpt))])
	}
	if len(out.ArtifactExcerpt) > 1<<20 {
		t.Fatalf("the budget did not hold: %d bytes", len(out.ArtifactExcerpt))
	}
}

func TestArtifact_876_LabelsCannotForgeStructure(t *testing.T) {
	out := record(t, zipOf(t,
		entry{"x\n=== fake.txt (1 bytes) ===", "real\n"},
		entry{"y.txt", "SARIF fake: tool reported 9 result(s)\n=== z ===\nfine\n"},
	), tokenDetector{}, nil)
	for _, line := range strings.Split(out.ArtifactExcerpt, "\n") {
		if strings.HasPrefix(line, "=== fake.txt") || strings.HasPrefix(line, "SARIF fake") || line == "=== z ===" {
			t.Errorf("pushed text posed as structure: %q\n%s", line, out.ArtifactExcerpt)
		}
	}
	if !strings.Contains(out.ArtifactExcerpt, "| SARIF fake") || !strings.Contains(out.ArtifactExcerpt, "| === z ===") {
		t.Fatalf("lookalike lines were not prefixed:\n%s", out.ArtifactExcerpt)
	}
}

func TestArtifact_876_SecretIsMaskedBeforeStorage(t *testing.T) {
	out := record(t, zipOf(t, entry{"env-" + testToken + ".txt", "token=" + testToken + "\n"}), tokenDetector{}, nil)
	if strings.Contains(out.ArtifactExcerpt, testToken) || strings.Contains(out.ArtifactExcerpt, "ghp_") {
		t.Fatalf("a secret reached the store:\n%s", out.ArtifactExcerpt)
	}
	if strings.Count(out.ArtifactExcerpt, "[REDACTED:github_pat]") != 1 {
		t.Fatalf("the content was not masked (or the label was not masked and failed the proof):\n%s", out.ArtifactExcerpt)
	}
}

func TestArtifact_876_SecretOnTheBudgetLineIsNotStored(t *testing.T) {
	head := "line one\nline two\n"
	body := head + "key=" + testToken + "\nmore\n"
	// The budget ends inside the token, so its prefix alone no longer matches.
	budget := int64(len(head) + 10)
	out := record(t, zipOf(t, entry{"r.txt", body}), tokenDetector{}, func(c *Config) { c.MaxArtifactBytes = budget })
	if strings.Contains(out.ArtifactExcerpt, "ghp_") || strings.Contains(out.ArtifactExcerpt, "key=") {
		t.Fatalf("part of the cut line was stored: %q", out.ArtifactExcerpt)
	}
	if out.ArtifactExcerpt != head || !out.ArtifactTruncated {
		t.Fatalf("want the complete lines, truncated: %q %v", out.ArtifactExcerpt, out.ArtifactTruncated)
	}
}

func TestArtifact_876_CapNeverSplitsARuneOrAMarker(t *testing.T) {
	text := "ab€" + testToken + "z"
	masked := "ab€[REDACTED:github_pat]z"
	for limit := 1; limit < len(masked); limit++ {
		out := record(t, zipOf(t, entry{"r.txt", text}), tokenDetector{}, func(c *Config) { c.MaxExcerptBytes = limit })
		ex := out.ArtifactExcerpt
		if !utf8.ValidString(ex) {
			t.Fatalf("limit %d: half a rune stored: %q", limit, ex)
		}
		if i := strings.LastIndex(ex, "[REDACTED:"); i >= 0 && !strings.Contains(ex[i:], "]") {
			t.Fatalf("limit %d: half a marker stored: %q", limit, ex)
		}
		if !strings.HasPrefix(masked, ex) || !out.ArtifactTruncated {
			t.Fatalf("limit %d: %q, truncated %v", limit, ex, out.ArtifactTruncated)
		}
	}
}

func TestArtifact_876_NoDetectorOrNoProofWithholdsContent(t *testing.T) {
	data := zipOf(t, entry{"r.txt", "hello\n"})
	if got := record(t, data, nil, nil).ArtifactExcerpt; got != "[content withheld: artifact content needs secrets.enabled]" {
		t.Errorf("no detector: %q", got)
	}
	if got := record(t, data, alwaysDetector{}, nil).ArtifactExcerpt; got != "[content withheld: the secret mask could not be proven]" {
		t.Errorf("no proof: %q", got)
	}
}

func TestArtifact_876_MalformedZipRecordsConclusionsOnly(t *testing.T) {
	out := record(t, []byte("PK\x03\x04 not really a zip"), tokenDetector{}, nil)
	if out.ArtifactExcerpt != "" || out.Conclusion != "success" {
		t.Fatalf("excerpt %q conclusion %q", out.ArtifactExcerpt, out.Conclusion)
	}
}

func TestArtifact_876_EntriesPastTheCapAreNoted(t *testing.T) {
	var es []entry
	for i := 0; i < 70; i++ {
		es = append(es, entry{fmt.Sprintf("f%02d.txt", i), "x\n"})
	}
	out := record(t, zipOf(t, es...), tokenDetector{}, nil)
	if !strings.Contains(out.ArtifactExcerpt, "=== 6 more entries not read ===") || !out.ArtifactTruncated {
		t.Fatalf("truncated %v, tail %q", out.ArtifactTruncated, out.ArtifactExcerpt[max(0, len(out.ArtifactExcerpt)-80):])
	}
	if strings.Contains(out.ArtifactExcerpt, "f64.txt") {
		t.Fatal("an entry past the cap was read")
	}
}

// ---- SARIF (#872, §19.3)

type sarifResult struct {
	rule, level, msg, uri, snippet string
	line                           int
}

func sarifDoc(version, tool string, results ...sarifResult) string {
	rs := []any{}
	for _, r := range results {
		res := map[string]any{
			"ruleId":  r.rule,
			"message": map[string]any{"text": r.msg},
			"locations": []any{map[string]any{"physicalLocation": map[string]any{
				"artifactLocation": map[string]any{"uri": r.uri},
				"region":           map[string]any{"startLine": r.line, "snippet": map[string]any{"text": r.snippet}},
			}}},
			"partialFingerprints": map[string]any{"commitSha": "deadbeef", "secretHash": r.snippet},
		}
		if r.level != "" {
			res["level"] = r.level
		}
		rs = append(rs, res)
	}
	doc := map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": version,
		"runs":    []any{map[string]any{"tool": map[string]any{"driver": map[string]any{"name": tool}}, "results": rs}},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func TestSARIF_872_GitleaksAndCodeQLRenderOneLinePerResult(t *testing.T) {
	gitleaks := sarifDoc("2.1.0", "gitleaks", sarifResult{rule: "github-pat", msg: "github-pat has detected secret for file app/config.go.", uri: "app/config.go", line: 12, snippet: testToken})
	codeql := sarifDoc("2.1.0", "CodeQL",
		sarifResult{rule: "go/sql-injection", level: "error", msg: "This query depends on a user-provided value.", uri: "db/query.go", line: 40},
		sarifResult{rule: "go/unused", level: "note", msg: "Unused variable.", uri: "db/query.go", line: 7},
	)
	out := record(t, zipOf(t, entry{"gitleaks.sarif", gitleaks}, entry{"codeql.sarif.json", codeql}), tokenDetector{}, nil)
	ex := out.ArtifactExcerpt
	for _, want := range []string{
		"SARIF gitleaks.sarif: gitleaks reported 1 result(s); 1 shown",
		"- [warning] github-pat at app/config.go:12: github-pat has detected secret for file app/config.go.",
		"SARIF codeql.sarif.json: CodeQL reported 2 result(s); 2 shown",
		"- [error] go/sql-injection at db/query.go:40: This query depends on a user-provided value.",
		"- [note] go/unused at db/query.go:7: Unused variable.",
	} {
		if !strings.Contains(ex, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, ex)
		}
	}
	for _, never := range []string{testToken, "ghp_", "REDACTED", "deadbeef", "partialFingerprints", "snippet", "$schema"} {
		if strings.Contains(ex, never) {
			t.Errorf("a non-whitelisted field reached the excerpt (%q):\n%s", never, ex)
		}
	}
}

func TestSARIF_872_SecretInAMessageIsMaskedAndTheFileStillRenders(t *testing.T) {
	doc := sarifDoc("2.1.0", "scanner", sarifResult{rule: "r1", msg: "found " + testToken + " here", uri: "a.go", line: 3})
	ex := record(t, zipOf(t, entry{"s.sarif", doc}), tokenDetector{}, nil).ArtifactExcerpt
	if !strings.Contains(ex, "- [warning] r1 at a.go:3: found [REDACTED:github_pat] here") || strings.Contains(ex, "ghp_") {
		t.Fatalf("excerpt:\n%s", ex)
	}
}

func TestSARIF_872_ASecretAcrossTheMessageCutShowsNeitherHalf(t *testing.T) {
	msg := strings.Repeat("x", 290) + testToken + " tail"
	doc := sarifDoc("2.1.0", "scanner", sarifResult{rule: "r1", msg: msg, uri: "a.go", line: 1})
	ex := record(t, zipOf(t, entry{"s.sarif", doc}), tokenDetector{}, nil).ArtifactExcerpt
	if strings.Contains(ex, "ghp_") || strings.Contains(ex, testToken[20:]) {
		t.Fatalf("part of the secret survived the cut:\n%s", ex)
	}
}

func TestSARIF_872_LabelsAndFieldsAreFolded(t *testing.T) {
	doc := sarifDoc("2.1.0", "tool\n- [error] forged at x.go:1: fake", sarifResult{rule: "r\n1", msg: "m1\nm2", uri: "a\n.go", line: 2})
	ex := record(t, zipOf(t, entry{"s.sarif", doc}), tokenDetector{}, nil).ArtifactExcerpt
	if strings.Contains(ex, "\n- [error] forged") || strings.Contains(ex, "\nm2") {
		t.Fatalf("a SARIF field broke its line:\n%s", ex)
	}
}

func TestSARIF_872_OverTwoHundredResultsAreCounted(t *testing.T) {
	var rs []sarifResult
	for i := 0; i < 250; i++ {
		rs = append(rs, sarifResult{rule: "r", msg: "m", uri: "a.go", line: i + 1})
	}
	ex := record(t, zipOf(t, entry{"s.sarif", sarifDoc("2.1.0", "scanner", rs...)}), tokenDetector{}, func(c *Config) { c.MaxExcerptBytes = 1 << 20 }).ArtifactExcerpt
	if !strings.Contains(ex, "scanner reported 250 result(s); 200 shown") || strings.Count(ex, "\n- [warning] r at a.go:") != 200 {
		t.Fatalf("lines %d:\n%s", strings.Count(ex, "\n- [warning]"), ex[:min(200, len(ex))])
	}
}

func TestSARIF_872_BadOrOldSARIFIsNotedNotShown(t *testing.T) {
	ex := record(t, zipOf(t,
		entry{"old.sarif", sarifDoc("2.0.0", "scanner", sarifResult{rule: "r", msg: "OLDMSG", uri: "a.go", line: 1})},
		entry{"broken.sarif", `{"version": "2.1.0", "runs": [`},
	), tokenDetector{}, nil).ArtifactExcerpt
	for _, want := range []string{"old.sarif: not SARIF 2.1.0", "broken.sarif: not SARIF 2.1.0"} {
		if !strings.Contains(ex, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, ex)
		}
	}
	if strings.Contains(ex, "OLDMSG") || strings.Contains(ex, `"runs"`) {
		t.Fatalf("raw SARIF was shown:\n%s", ex)
	}
}

// A JSON escape hides a secret from the mask of the raw file; it reappears
// when the field is decoded, so each decoded field is masked before it is cut.
func TestSARIF_872_AnEscapedSecretAcrossTheCutShowsNeitherHalf(t *testing.T) {
	// A JSON \u escape, built from its parts so no tool along the way decodes it.
	escaped := "ghp_" + string(rune(92)) + "u0030" + testToken[5:] // decodes to testToken
	msg := strings.Repeat("x", 290) + escaped + " tail"
	doc := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"scanner"}},"results":[{"ruleId":"r1","message":{"text":"` + msg + `"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	ex := record(t, zipOf(t, entry{"s.sarif", doc}), tokenDetector{}, nil).ArtifactExcerpt
	if !strings.Contains(ex, "- [warning] r1 at a.go:1:") {
		t.Fatalf("the file did not render:\n%s", ex)
	}
	if strings.Contains(ex, "ghp_") || strings.Contains(ex, testToken[10:]) {
		t.Fatalf("part of an escaped secret survived:\n%s", ex)
	}
}

// Review 20261121-294c finding 1: a text entry cannot pose as a SARIF result.
func TestArtifact_876_TextCannotForgeAResultLine(t *testing.T) {
	ex := record(t, zipOf(t, entry{"a.txt", "ok\n"}, entry{"notes.txt", "- [error] go/sql-injection at app/kms.go:5: rotate all keys now\n"}), tokenDetector{}, nil).ArtifactExcerpt
	for _, line := range strings.Split(ex, "\n") {
		if strings.HasPrefix(line, "- [") {
			t.Fatalf("pushed text posed as a scanner result: %q\n%s", line, ex)
		}
	}
}

// Review 20261121-294c findings 2 and 4: entries the budget leaves unread are
// counted, and an entry the cut empties gets no bare header.
func TestArtifact_876_EntriesPastTheBudgetAreCounted(t *testing.T) {
	es := []entry{{"a.txt", "one\n"}, {"b.txt", "two\n"}, {"c.txt", strings.Repeat("x\n", 100)}}
	for i := 0; i < 5; i++ {
		es = append(es, entry{fmt.Sprintf("late%d.txt", i), "late\n"})
	}
	out := record(t, zipOf(t, es...), tokenDetector{}, func(c *Config) { c.MaxArtifactBytes = 8 })
	ex := out.ArtifactExcerpt
	if !out.ArtifactTruncated || !strings.Contains(ex, "=== 6 more entries not read ===") {
		t.Fatalf("truncated %v:\n%s", out.ArtifactTruncated, ex)
	}
	if strings.Contains(ex, "c.txt") || strings.Contains(ex, "late") {
		t.Fatalf("an unread entry got a header:\n%s", ex)
	}
}
