package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #626 (agent-administered Vornik design, amendment 2026-11-12 A, B):
// configuration placeholders fall back to the stored operator credentials.

func writeCredConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte("api:\n  auth_enabled: false\n"+body), 0o600))
	return p
}

const credDB626 = `database:
  driver: sqlite
  path: "${DBPATH_626}"
`

func TestLoadBootstrap_ARequiredFieldOnlyTheStoreFillsDoesNotFail_626(t *testing.T) {
	t.Setenv("DBPATH_626", "/tmp/x.db")
	t.Setenv("ADDR_626", "")
	p := writeCredConfig(t, credDB626+"server:\n  address: \"${ADDR_626}\"\n")

	_, _, err := LoadFromPathWithProvenance(p)
	require.Error(t, err, "the full load still requires server.address")
	assert.Contains(t, err.Error(), "ADDR_626")

	cfg, _, err := LoadFromPathWithProvenance(p, func(o *loadOptions) { o.bootstrap = true })
	require.NoError(t, err, "the bootstrap load validates only the database")
	assert.Equal(t, []string{"ADDR_626"}, cfg.CredentialCandidates(), "a name referenced only under database: is never a candidate")
}

func TestLoadBootstrap_StillValidatesTheDatabase_626(t *testing.T) {
	t.Setenv("DBPATH_626", "")
	p := writeCredConfig(t, credDB626)
	_, _, err := LoadFromPathWithProvenance(p, func(o *loadOptions) { o.bootstrap = true })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database path is required")
}

func TestWithCredentials_FillsWhatTheEnvironmentLacks_626(t *testing.T) {
	t.Setenv("DBPATH_626", "/tmp/x.db")
	t.Setenv("ADDR_626", "")
	p := writeCredConfig(t, credDB626+"server:\n  address: \"${ADDR_626}\"\n")

	cfg, prov, err := LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"ADDR_626": ":9626"}))
	require.NoError(t, err)
	assert.Equal(t, ":9626", cfg.Server.Address)
	v := prov.Values["server.address"]
	assert.Equal(t, OriginCredential, v.Origin)
	assert.Equal(t, "${ADDR_626}", v.Source, "provenance names the variable, never the value")
	assert.NotContains(t, v.Source, "9626")
}

func TestWithCredentials_TheEnvironmentWinsAndAShadowIsReported_626(t *testing.T) {
	t.Setenv("DBPATH_626", "/tmp/x.db")
	t.Setenv("ADDR_626", ":7000")
	p := writeCredConfig(t, credDB626+"server:\n  address: \"${ADDR_626}\"\n")

	cfg, prov, err := LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"ADDR_626": ":9626"}))
	require.NoError(t, err)
	assert.Equal(t, ":7000", cfg.Server.Address)
	assert.Equal(t, []string{"ADDR_626"}, cfg.ShadowedCredentials())
	v := prov.Values["server.address"]
	assert.Equal(t, OriginPlaceholder, v.Origin)
	assert.Contains(t, v.Source, "shadows a stored credential")

	// The same value in both is not a shadow.
	cfg, _, err = LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"ADDR_626": ":7000"}))
	require.NoError(t, err)
	assert.Empty(t, cfg.ShadowedCredentials())
}

func TestWithCredentials_NeverFillsTheDatabaseBlock_626(t *testing.T) {
	t.Setenv("DBPATH_626", "")
	p := writeCredConfig(t, credDB626)
	_, _, err := LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"DBPATH_626": "/stolen.db"}))
	require.Error(t, err, "a stored value never reaches database:")
	assert.Contains(t, err.Error(), "database path is required")
}

func TestWithCredentials_AStoredValueIsNotExpandedAgain_626(t *testing.T) {
	t.Setenv("DBPATH_626", "/tmp/x.db")
	t.Setenv("ADDR_626", "")
	t.Setenv("X", "expanded")
	p := writeCredConfig(t, credDB626+"server:\n  address: \"${ADDR_626}\"\n")
	cfg, _, err := LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"ADDR_626": "a${X}$b"}))
	require.NoError(t, err)
	assert.Equal(t, "a${X}$b", cfg.Server.Address)
}

func TestWithCredentials_ANameAbsentEverywhereStaysEmptyAndIsNamed_626(t *testing.T) {
	t.Setenv("DBPATH_626", "/tmp/x.db")
	t.Setenv("ADDR_626", "")
	p := writeCredConfig(t, credDB626+"server:\n  address: \"${ADDR_626}\"\n")
	_, _, err := LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"OTHER": "v"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "${ADDR_626}", "the unresolved name is in the error")
	_, _, err = LoadFromPathWithProvenance(p, WithCredentials(map[string]string{"ADDR_626": ":1"}))
	require.NoError(t, err, "a stored name is not reported unresolved")
}

func TestCredentialCandidates_OnlyValidNamesOutsideDatabase_626(t *testing.T) {
	raw := []byte("database:\n  password: \"${DB_PW}\"\nchat:\n  api_key: \"${CHAT_KEY}\"\n  model: \"${lower_case}\"\nx: \"${CHAT_KEY}${B_2}\"\n")
	assert.Equal(t, []string{"B_2", "CHAT_KEY"}, credentialCandidateNames(raw))
	assert.True(t, strings.Contains(string(raw), "DB_PW"))
}

// Review a3dc F3: comments and mapping keys are not references.
func TestCredentialCandidates_IgnoreCommentsAndKeys_626(t *testing.T) {
	raw := []byte("chat:\n  api_key: \"${REAL_626}\" # was ${OLD_626}\n  # ${COMMENTED_626}\nmap:\n  \"${KEY_626}\": v\nlist:\n  - \"${LISTED_626}\"\n")
	assert.Equal(t, []string{"LISTED_626", "REAL_626"}, credentialCandidateNames(raw))
	assert.Empty(t, DatabaseNames(raw))
	assert.Equal(t, []string{"DB_626"}, DatabaseNames([]byte("database:\n  password: \"${DB_626}\" # ${NOT_626}\n")))
}
