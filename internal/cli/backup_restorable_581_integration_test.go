//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// GitHub #581 D4, through the real runBackup and pg_dump: --restorable adds
// secrets/ beside the config file, the ordinary backup does not, and every
// archive is written 0600 (it holds the database and, managed, the config's
// literal keys).
func TestIntegration_RunBackup581_RestorableCarriesSecretsAt0600(t *testing.T) {
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

	for _, restorable := range []bool{false, true} {
		backupInt_reset()
		backupRestorable = restorable
		backupOut = filepath.Join(t.TempDir(), "backup.tgz")
		if out, err := dbcovCapture(t, func() error { return runBackup(backupCmd, nil) }); err != nil {
			t.Fatalf("runBackup(restorable=%v): %v\n%s", restorable, err, out)
		}
		info, err := os.Stat(backupOut)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("restorable=%v: archive mode %v (%v); want 0600", restorable, info, err)
		}
		if got := backupIntListTar(t, backupOut)["secrets/store.key"]; got != restorable {
			t.Errorf("restorable=%v: secrets/store.key in the archive = %v", restorable, got)
		}
	}
	backupRestorable = false
}
