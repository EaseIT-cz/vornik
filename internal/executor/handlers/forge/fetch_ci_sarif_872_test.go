package forge

// GitHub #872 and #876, CI outcomes design §19.6 (integration): a ZIP'd
// gitleaks SARIF artifact goes through the real forgeci ingest, with the
// daemon's real secret detector, into a real SQLite outcome store, and
// forge.fetch_ci renders the scanner's results inside the untrusted wrapper,
// with the secret the scanner found nowhere in the store or the prompt.
//
// It sits here, not in internal/executor's system-chain test, because package
// executor cannot import the forge handlers (they import it).

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secrets"
)

type sarifZipReader struct{ data []byte }

func (r sarifZipReader) FetchCIRun(context.Context, string, int64) (forge.CIRun, error) {
	return forge.CIRun{}, nil
}
func (r sarifZipReader) ListCIArtifacts(context.Context, string, int64) ([]forge.CIArtifact, error) {
	return []forge.CIArtifact{{ID: 9, Name: "scan"}}, nil
}
func (r sarifZipReader) FetchCIArtifact(context.Context, string, int64, int64) ([]byte, error) {
	return r.data, nil
}
func (r sarifZipReader) VerifyCIAccess(context.Context) error { return nil }

func TestFetchCI_872_ZippedSARIFReachesTheReviewMasked(t *testing.T) {
	// A GitHub personal access token shape the default patterns match.
	leaked := "ghp_" + strings.Repeat("Ab1", 12)
	doc := map[string]any{
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool": map[string]any{"driver": map[string]any{"name": "gitleaks"}},
			"results": []any{map[string]any{
				"ruleId":  "github-pat",
				"message": map[string]any{"text": "github-pat has detected secret for file app/config.go."},
				"locations": []any{map[string]any{"physicalLocation": map[string]any{
					"artifactLocation": map[string]any{"uri": "app/config.go"},
					"region":           map[string]any{"startLine": 12, "snippet": map[string]any{"text": "token = \"" + leaked + "\""}},
				}}},
			}},
		}},
	}
	raw, _ := json.Marshal(doc)
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("gitleaks.sarif")
	_, _ = f.Write(raw)
	n, _ := w.Create("notes.txt")
	_, _ = n.Write([]byte("scanner token was " + leaked + "\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Scan([]byte(leaked))) == 0 {
		t.Fatal("fixture: the default detector does not match the planted token")
	}

	db := sqlitetest.Memory(t)
	store := sqlite.NewForgeCIOutcomeRepository(db.DB)
	ing := forgeci.New(store, sarifZipReader{data: buf.Bytes()}, forgeci.Config{
		ArtifactName: "scan", MaxArtifactBytes: 1 << 20, MaxExcerptBytes: 64 << 10, Detector: d,
	}, zerolog.Nop())
	if out := ing.Record(context.Background(), forgeci.Run{
		ProjectID: "p-1", Repo: "acme/infra", RunID: 77, HeadSHA: "sha-872", Number: 42,
		Conclusion: "success", WorkflowPath: ".github/workflows/scan.yml",
	}); out == nil {
		t.Fatal("nothing recorded")
	}

	rows, err := store.ListByHeadSHA(context.Background(), "p-1", "acme/infra", "sha-872")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored rows: %d, %v", len(rows), err)
	}
	if strings.Contains(rows[0].ArtifactExcerpt, leaked) {
		t.Fatalf("the secret reached the store:\n%s", rows[0].ArtifactExcerpt)
	}

	res, err := NewFetchCIHandler(store).Execute(context.Background(), ciStepInput(t, "sha-872"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(res.Result, &payload); err != nil {
		t.Fatal(err)
	}
	msg := payload.Message
	line := "- [warning] github-pat at app/config.go:12: github-pat has detected secret for file app/config.go."
	i := strings.Index(msg, line)
	if i < 0 || !strings.Contains(msg, "SARIF gitleaks.sarif: gitleaks reported 1 result(s); 1 shown") {
		t.Fatalf("the scanner's result is not in the review's CI section:\n%s", msg)
	}
	if open := strings.LastIndex(msg[:i], "<untrusted_content"); open < 0 || !strings.Contains(msg[i:], "</untrusted_content>") {
		t.Fatalf("the scanner's result is outside the untrusted wrapper:\n%s", msg)
	}
	if strings.Contains(msg, leaked) || !strings.Contains(msg, "[REDACTED:") {
		t.Fatalf("the prompt holds the secret, or the note's copy was not masked:\n%s", msg)
	}
}
