package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBackupClientVersionDiagnostic reproduces customer issue #68
// (2026-10-06): PATH selects PG15 pg_dump against a PG16 server. Exercise
// the config -> subprocess -> operator-error seam without a database.
func TestRunBackupClientVersionDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic, hint string
	}{
		{"pg16", "pg_dump: error: aborting because of server version mismatch\npg_dump: detail: server version: 16.15 (Debian); pg_dump version: 15.18 (Debian)", "postgresql-client-16"},
		{"unknown", "pg_dump: error: aborting because of server version mismatch", "matching or newer PostgreSQL client"},
		{"ordinary", "pg_dump: error: connection refused", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte("api:\n  auth_enabled: false\ndatabase:\n  host: localhost\n  port: 5432\n  user: test\n  name: test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("VORNIK_CONFIG", configPath)
			t.Setenv("PATH", dir)
			script := "#!/bin/sh\nprintf '%s\\n' '" + tc.diagnostic + "' >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(dir, "pg_dump"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			oldOut := backupOut
			backupOut = filepath.Join(dir, "backup.tgz")
			t.Cleanup(func() { backupOut = oldOut })
			err := runBackup(backupCmd, nil)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("expected wrapped subprocess failure, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("lost pg_dump diagnostic: %v", err)
			}
			if tc.hint != "" && (!strings.Contains(err.Error(), tc.hint) || !strings.Contains(err.Error(), "PATH")) {
				t.Fatalf("missing actionable client guidance: %v", err)
			}
			if tc.hint == "" && strings.Contains(err.Error(), "PostgreSQL client") {
				t.Fatalf("unrelated error received mismatch guidance: %v", err)
			}
			if _, err := os.Stat(backupOut); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed dump created archive: %v", err)
			}
		})
	}
}
