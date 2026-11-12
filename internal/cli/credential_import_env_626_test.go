package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #626 C: vornikctl credential import-env.

func importEnvFixture(t *testing.T) (configPath, secrets string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`database:
  password: "${POSTGRES_PASSWORD}"
chat:
  api_key: "${CHAT_KEY}"
telegram:
  bot_token: "${TG_TOKEN}"
`), 0o600))
	secrets = filepath.Join(dir, "secrets")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "configs", "projects"), 0o700))
	require.NoError(t, os.MkdirAll(secrets, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "configs", "projects", "p.yaml"), []byte("email:\n  imap_password_env: IMAP_P\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "chat.env"), []byte("# chat\nCHAT_KEY=sk-chat-secret\nUNUSED=nobody-reads-me\nPOSTGRES_PASSWORD=db-pw\nIMAP_P='imap-secret'\nlower=x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "project-secrets.env"), []byte("SLACK_SIGN_P=slack-secret\n"), 0o600))
	return configPath, secrets
}

const importMove = "move"

func actions(p importEnvPlan) map[string]string {
	m := map[string]string{}
	for _, f := range p.files {
		for _, e := range f.Entries {
			if e.Move {
				m[e.Key] = importMove
			} else {
				m[e.Key] = e.Reason
			}
		}
	}
	return m
}

func TestImportEnvPlan_SelectsWhatTheConfigUses_626(t *testing.T) {
	configPath, _ := importEnvFixture(t)
	plan, err := buildImportEnvPlan(configPath)
	require.NoError(t, err)
	a := actions(plan)
	assert.Equal(t, importMove, a["CHAT_KEY"], "referenced in config.yaml")
	assert.Equal(t, importMove, a["IMAP_P"], "a project *_env field")
	assert.Equal(t, importMove, a["SLACK_SIGN_P"], "in project-secrets.env")
	assert.Contains(t, a["POSTGRES_PASSWORD"], "database", "never a database: name")
	assert.Contains(t, a["UNUSED"], "not referenced")
	assert.Contains(t, a["lower"], "invalid name")

	var out bytes.Buffer
	moves := plan.print(&out)
	assert.Equal(t, 3, moves)
	for _, v := range []string{"sk-chat-secret", "nobody-reads-me", "db-pw", "imap-secret", "slack-secret"} {
		assert.NotContains(t, out.String(), v, "the plan never prints a value")
	}
	assert.Contains(t, out.String(), "chat.env:2")
}

func TestImportEnvApply_MovesExactlyTheStoredLines_626(t *testing.T) {
	configPath, secrets := importEnvFixture(t)
	plan, err := buildImportEnvPlan(configPath)
	require.NoError(t, err)
	got := map[string]string{}
	stored, report := applyImportEnvPlan(plan, func(n string, v []byte) error {
		if n == "IMAP_P" {
			return errors.New("refused")
		}
		got[n] = string(v)
		return nil
	})
	assert.Equal(t, []string{"CHAT_KEY", "SLACK_SIGN_P"}, stored)
	assert.Equal(t, "sk-chat-secret", got["CHAT_KEY"])
	require.Len(t, report, 1)
	assert.Contains(t, report[0], "IMAP_P: not stored")

	chat, err := os.ReadFile(filepath.Join(secrets, "chat.env"))
	require.NoError(t, err)
	assert.Equal(t, "# chat\nUNUSED=nobody-reads-me\nPOSTGRES_PASSWORD=db-pw\nIMAP_P='imap-secret'\nlower=x\n", string(chat),
		"only the stored line goes; the failed PUT, the database name, unreferenced and invalid names stay")
	fi, err := os.Stat(filepath.Join(secrets, "chat.env"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	ps, err := os.ReadFile(filepath.Join(secrets, "project-secrets.env"))
	require.NoError(t, err)
	assert.Empty(t, string(ps))
}

func TestImportEnvApply_AFileChangedBeforeTheRenameIsLeftAndReported_626(t *testing.T) {
	configPath, secrets := importEnvFixture(t)
	plan, err := buildImportEnvPlan(configPath)
	require.NoError(t, err)
	chatPath := filepath.Join(secrets, "chat.env")
	orig := importEnvBeforeRename
	t.Cleanup(func() { importEnvBeforeRename = orig })
	importEnvBeforeRename = func(p string) {
		if p == chatPath {
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, _ = f.WriteString("NEW=edited-meanwhile\n")
			_ = f.Close()
		}
	}
	_, report := applyImportEnvPlan(plan, func(string, []byte) error { return nil })
	joined := strings.Join(report, "\n")
	assert.Contains(t, joined, "changed during import, re-run")
	chat, err := os.ReadFile(chatPath)
	require.NoError(t, err)
	assert.Contains(t, string(chat), "CHAT_KEY=sk-chat-secret", "the file is left as it was, plus the edit")
	assert.Contains(t, string(chat), "NEW=edited-meanwhile")
}

func TestImportEnvApply_ALaterLineWithAnotherValueStays_626(t *testing.T) {
	configPath, secrets := importEnvFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "zz.env"), []byte("CHAT_KEY=a-different-value\n"), 0o600))
	plan, err := buildImportEnvPlan(configPath)
	require.NoError(t, err)
	var gotValue string
	_, report := applyImportEnvPlan(plan, func(n string, v []byte) error {
		if n == "CHAT_KEY" {
			gotValue = string(v)
		}
		return nil
	})
	assert.Equal(t, "sk-chat-secret", gotValue, "the first file's value, the one the daemon uses")
	zz, _ := os.ReadFile(filepath.Join(secrets, "zz.env"))
	assert.Equal(t, "CHAT_KEY=a-different-value\n", string(zz))
	assert.Contains(t, strings.Join(report, "\n"), "the value differs from the stored one")
	for _, r := range report {
		assert.NotContains(t, r, "a-different-value")
	}
}

// Review d2bc F1: within one file the loader keeps a key's last line, so the
// store gets that value and the earlier line stays.
func TestImportEnvApply_ARepeatedKeyInOneFileStoresTheLastValue_626(t *testing.T) {
	configPath, secrets := importEnvFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "chat.env"), []byte("CHAT_KEY=v1\nCHAT_KEY=v2\n"), 0o600))
	plan, err := buildImportEnvPlan(configPath)
	require.NoError(t, err)
	var got string
	_, _ = applyImportEnvPlan(plan, func(n string, v []byte) error {
		if n == "CHAT_KEY" {
			got = string(v)
		}
		return nil
	})
	assert.Equal(t, "v2", got, "the value the daemon uses")
	chat, _ := os.ReadFile(filepath.Join(secrets, "chat.env"))
	assert.Equal(t, "CHAT_KEY=v1\n", string(chat), "the overridden line stays")
}
