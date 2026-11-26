package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #864: the replay copy drops the two statements a restore must not
// replay (COMMENT ON EXTENSION, which only the extension's owner may run, and
// pg_dump 17's SET transaction_timeout, which a 16 server refuses) and keeps
// everything else byte for byte, comments on vornik's own objects included.
//
// Found at: production. Regression test at: unit (the filter); the restore
// itself is restore_clean_864_integration_test.go. Lower because: the filter
// is pure text.
func TestWriteRestoreDump_DropsOnlyTheTwoStatements_864(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "db.sql")
	long := strings.Repeat("x", 200000) // a COPY row longer than bufio.Scanner's limit
	dump := "SET statement_timeout = 0;\n" +
		"SET transaction_timeout = 0;\n" +
		"CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;\n" +
		"COMMENT ON EXTENSION vector IS 'vector data type and ivfflat and hnsw access methods';\n" +
		"COMMENT ON TABLE public.tasks IS 'kept';\n" +
		"COMMENT ON COLUMN public.tasks.error_code IS 'Typed failure class';\n" +
		"-- COMMENT ON EXTENSION in a comment line stays\n" +
		long + "\n" +
		"SELECT 1;" // no trailing newline
	if err := os.WriteFile(in, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "db.restore.sql")
	if err := writeRestoreDump(in, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"SET transaction_timeout = 0;\n", "",
		"COMMENT ON EXTENSION vector IS 'vector data type and ivfflat and hnsw access methods';\n", "",
	).Replace(dump)
	if string(got) != want {
		t.Errorf("replay copy differs from the dump minus the two statements (len %d, want %d)", len(got), len(want))
	}
}

// pg_dump writes a comment verbatim, so an extension's comment can span
// lines; the whole statement goes, and only it (review of the #864 design,
// CRIT-1). A quote inside the text, written as two apostrophes, does not end the literal.
func TestWriteRestoreDump_DropsAMultiLineExtensionComment_864(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "db.sql")
	dump := "COMMENT ON EXTENSION vector IS 'two\nlines; it''s\nthree';\nCREATE TABLE kept (id int);\n"
	if err := os.WriteFile(in, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.sql")
	if err := writeRestoreDump(in, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "CREATE TABLE kept (id int);\n" {
		t.Errorf("replay copy = %q", got)
	}
}

func TestWriteRestoreDump_RefusesADumpEndingInsideAnExtensionComment_864(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "db.sql")
	if err := os.WriteFile(in, []byte("SELECT 1;\nCOMMENT ON EXTENSION vector IS 'never\nclosed;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeRestoreDump(in, filepath.Join(dir, "out.sql"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want a refusal naming line 2, got %v", err)
	}
}
