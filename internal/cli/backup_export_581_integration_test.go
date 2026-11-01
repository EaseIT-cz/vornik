//go:build integration

package cli

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #581 D5, through the real runBackup and pg_dump: an owner export
// holds no secrets/, and its dump holds no rows of any SecretMaterial table
// while the owner's tables keep theirs.
func TestIntegration_RunBackup581_ExportLeavesOutSecretMaterial(t *testing.T) {
	if _, err := os.Stat("/usr/bin/pg_dump"); err != nil {
		t.Skip("pg_dump unavailable")
	}
	dbcovSetup(t)
	secrets := filepath.Join(filepath.Dir(os.Getenv("VORNIK_CONFIG")), "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "store.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	backupInt_reset()
	backupExport = true
	defer func() { backupExport = false }()
	backupOut = filepath.Join(t.TempDir(), "export.tgz")
	out, err := dbcovCapture(t, func() error { return runBackup(backupCmd, nil) })
	if err != nil {
		t.Fatalf("runBackup(--export): %v\n%s", err, out)
	}
	if !strings.Contains(out, "dump scrubbed by value") {
		t.Errorf("the export did not scrub its dump by value:\n%s", out)
	}
	if !strings.Contains(out, "secret-material table(s) left out") {
		t.Errorf("the export does not print what it left out:\n%s", out)
	}
	names := backupIntListTar(t, backupOut)
	for name := range names {
		if strings.HasPrefix(name, "secrets") {
			t.Errorf("the export holds %s", name)
		}
	}
	dump := readArchiveFile581(t, backupOut, "db.sql")
	for _, table := range persistence.SecretMaterialTables() {
		if strings.Contains(dump, "COPY public."+table+" ") {
			t.Errorf("the dump carries rows of the SecretMaterial table %s", table)
		}
		if !strings.Contains(dump, "CREATE TABLE public."+table+" ") {
			t.Errorf("the dump lost the schema of %s", table)
		}
	}
	if !strings.Contains(dump, "COPY public.tasks ") {
		t.Error("the dump lost the owner's tasks")
	}
}

func readArchiveFile581(t *testing.T, archive, name string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("%s not in the archive", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == name {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
}
