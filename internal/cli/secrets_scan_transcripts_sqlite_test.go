package cli

// GitHub #414: the transcript rescan suite on SQLite (the default lane). The
// Postgres half is secrets_scan_transcripts_integration_test.go.

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secrets"
)

// The two non-pattern rules are nameable, secret_field is strong and cut_token
// is heuristic (design §10.2.1).
func TestRuleSelection414_NonPatternRules(t *testing.T) {
	corpus := secrets.EffectivePatterns(nil, nil)
	strong := strong414(t)
	if !strong.selects(auditredact.FindingSecretField) || strong.selects(auditredact.FindingCutToken) {
		t.Fatal("strong must select secret_field and not cut_token")
	}
	sel, err := parseRuleSelection("cut_token,secret_field", corpus)
	if err != nil || !sel.selects(auditredact.FindingCutToken) || sel.selects("aws_access_key") {
		t.Fatalf("explicit names: %+v %v", sel, err)
	}
}

func TestScanTranscripts414_SQLite(t *testing.T) {
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	runTranscriptRescanSuite(t, transcriptRig{
		env: sqliteScanEnv(db.DB),
		createTask: func(t *testing.T, id, project string) {
			t.Helper()
			if err := tasks.Create(context.Background(), newTask414(id, project)); err != nil {
				t.Fatalf("create task: %v", err)
			}
		},
		orphans: true,
	}, "scan414-lite")
}
