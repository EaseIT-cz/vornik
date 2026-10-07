package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/secrets"
)

// TestIngestDocumentGoogleLinksPersistWithCredentialsRedacted reproduces
// customer issue #69 (2026-10-06): entropy redaction destroyed the document
// URL before chunk persistence. Assert actual repository INSERT content,
// including real credential redaction through the same production seam.
func TestIngestDocumentGoogleLinksPersistWithCredentialsRedacted(t *testing.T) {
	const id = "1aB2cD3eF4gH5iJ6kL7mN8oP9qR0sT_uVwXyZabcdEfGh"
	const key = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	for _, link := range []string{
		"https://docs.google.com/spreadsheets/d/" + id + "/edit",
		"https://docs.google.com/document/d/" + id + "/edit",
		"https://drive.google.com/file/d/" + id + "/view",
		"https://drive.google.com/drive/folders/" + id,
		"https://sheets.google.com/spreadsheets/d/" + id + "/edit",
	} {
		t.Run(link, func(t *testing.T) {
			idx, mock, cleanup := newTestIndexer(t)
			defer cleanup()
			det, err := secrets.NewMultiDetector(secrets.Config{})
			require.NoError(t, err)
			idx.SetSecrets(det, nil)
			want := "Document tracker " + link + "\nAPI key " + "[REDACTED:openai_key]"
			mock.ExpectExec("INSERT INTO project_memory_chunks").WithArgs(
				sqlmock.AnyArg(), "p", "t", "a", "tracker.md", 0, want,
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, idx.IngestDocumentText(context.Background(), "p", "t", "a", "tracker.md", "Document tracker "+link+"\nAPI key "+key))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
