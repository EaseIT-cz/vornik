package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/storage"
)

// GitHub #626 A (agent-administered Vornik design, amendment 2026-11-12):
// the daemon's boot reads the operator credentials the configuration names,
// between the bootstrap load and the full load.

type fakeCredentialReader struct {
	values map[string]string
	err    error
	asked  []string
}

func (f *fakeCredentialReader) GetOperatorCredential(_ context.Context, name string) ([]byte, error) {
	f.asked = append(f.asked, name)
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.values[name]
	if !ok {
		return nil, secretstore.ErrNotFound
	}
	return []byte(v), nil
}

func TestReadStoredCredentials_626(t *testing.T) {
	r := &fakeCredentialReader{values: map[string]string{"A": "1", "EMPTY": ""}}
	found, err := readStoredCredentials(context.Background(), r, []string{"A", "MISSING", "EMPTY"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1"}, found, "a missing or empty row is left out")

	r = &fakeCredentialReader{err: errors.New("db down")}
	_, err = readStoredCredentials(context.Background(), r, []string{"A"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential A")
}

// Review a3dc F1: a read that fails partway returns no map, so the boot never
// loads a partly filled configuration.
func TestReadStoredCredentials_AFailurePartwayReturnsNothing_626(t *testing.T) {
	r := &failAfter626{values: map[string]string{"A": "1", "B": "2"}, failOn: "B"}
	found, err := readStoredCredentials(context.Background(), r, []string{"A", "B", "C"})
	require.Error(t, err)
	assert.Nil(t, found, "the names read before the failure are discarded")
}

type failAfter626 struct {
	values map[string]string
	failOn string
}

func (f *failAfter626) GetOperatorCredential(_ context.Context, name string) ([]byte, error) {
	if name == f.failOn {
		return nil, errors.New("connection reset")
	}
	return []byte(f.values[name]), nil
}

// Review a3dc F2: the reload reads the running store. A credential stored
// after start fills a placeholder at the next reload.
func TestStageConfigForReload_ACredentialStoredAfterStartResolves_626(t *testing.T) {
	configPath, db := bootFixture(t, "OTHER_626", "x")
	t.Setenv("RELOAD_626", "")
	body := "api:\n  auth_enabled: false\ndatabase:\n  driver: sqlite\n  path: " + db.Path + "\nserver:\n  address: \"${RELOAD_626}\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))

	ctx := sqlitetest.Context(t)
	backend, err := storage.Open(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	key, err := secretstore.LoadKey(filepath.Join(filepath.Dir(configPath), "secrets", "store.key"))
	require.NoError(t, err)
	st, err := secretstore.New(backend.Repos.AgentSecrets, key)
	require.NoError(t, err)

	c := &Container{ConfigPath: configPath, Logger: zerolog.Nop()}
	c.setSecretStore(st)
	_, _, err = c.stageConfigForReload()
	require.Error(t, err, "not stored yet: the required field is empty")

	require.NoError(t, st.PutOperatorCredential(ctx, "RELOAD_626", []byte(":9627"), "console"))
	staged, prov, err := c.stageConfigForReload()
	require.NoError(t, err)
	assert.Equal(t, ":9627", staged.Server.Address)
	assert.Equal(t, config.OriginCredential, prov.Values["server.address"].Origin)
}

// Review a3dc F6: the boot's store failure is logged with the names, never a
// value.
func TestLogBootCredentials_AStoreFailureIsLogged_626(t *testing.T) {
	var buf logBuffer
	c := &Container{Logger: zerolog.New(&buf)}
	c.logBootCredentials(bootCredentials{err: errors.New("the store key: missing"), names: []string{"CHAT_KEY_626"}}, nil)
	assert.Contains(t, buf.String(), "the credential store could not be read")
	assert.Contains(t, buf.String(), "CHAT_KEY_626")
	assert.Contains(t, buf.String(), "the store key: missing")
}

// bootFixture is a config dir with a SQLite database holding one operator
// credential, sealed under the dir's store key.
func bootFixture(t *testing.T, name, value string) (configPath string, db config.DatabaseConfig) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	db = config.DatabaseConfig{Driver: "sqlite", Path: filepath.Join(dir, "vornik.db")}
	ctx := sqlitetest.Context(t)
	backend, err := storage.Open(ctx, db)
	require.NoError(t, err)
	key, err := secretstore.LoadOrCreateKey(filepath.Join(dir, "secrets", "store.key"))
	require.NoError(t, err)
	st, err := secretstore.New(backend.Repos.AgentSecrets, key)
	require.NoError(t, err)
	require.NoError(t, st.PutOperatorCredential(ctx, name, []byte(value), "test"))
	require.NoError(t, backend.Close())
	return configPath, db
}

func TestStoredCredentialsForBoot_ReadsTheStore_626(t *testing.T) {
	configPath, db := bootFixture(t, "CHAT_KEY_626", "sk-stored")
	boot := &config.Config{Database: db}

	got := storedCredentialsForBoot(sqlitetest.Context(t), boot, configPath, []string{"CHAT_KEY_626", "ABSENT_626"})
	require.NoError(t, got.err)
	assert.Equal(t, map[string]string{"CHAT_KEY_626": "sk-stored"}, got.found)

	none := storedCredentialsForBoot(sqlitetest.Context(t), boot, configPath, nil)
	assert.Nil(t, none.found, "no names: nothing opened, nothing read")
	assert.NoError(t, none.err)
}

func TestStoredCredentialsForBoot_AWrongKeyReadsNothingAndSaysWhy_626(t *testing.T) {
	configPath, db := bootFixture(t, "CHAT_KEY_626", "sk-stored")
	keyPath := filepath.Join(filepath.Dir(configPath), "secrets", "store.key")
	other, err := secretstore.LoadOrCreateKey(filepath.Join(t.TempDir(), "other.key"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, other, 0o600))

	got := storedCredentialsForBoot(sqlitetest.Context(t), &config.Config{Database: db}, configPath, []string{"CHAT_KEY_626"})
	assert.Empty(t, got.found)
	require.Error(t, got.err)

	require.NoError(t, os.Remove(keyPath))
	got = storedCredentialsForBoot(sqlitetest.Context(t), &config.Config{Database: db}, configPath, []string{"CHAT_KEY_626"})
	require.Error(t, got.err)
	assert.Contains(t, got.err.Error(), "store key")
}

// The boot chain end to end on a real store: a required field whose
// placeholder only the store holds loads; with the store unreadable, the
// full load fails naming the placeholder.
func TestBootChain_APlaceholderOnlyTheStoreHoldsLoads_626(t *testing.T) {
	configPath, db := bootFixture(t, "ADDR_BOOT_626", ":9626")
	t.Setenv("ADDR_BOOT_626", "")
	body := "api:\n  auth_enabled: false\ndatabase:\n  driver: sqlite\n  path: " + db.Path + "\nserver:\n  address: \"${ADDR_BOOT_626}\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))

	boot, err := config.LoadBootstrapFromPath(configPath)
	require.NoError(t, err)
	stored := storedCredentialsForBoot(sqlitetest.Context(t), boot, configPath, boot.CredentialCandidates())
	require.NoError(t, stored.err)
	cfg, prov, err := config.LoadFromPathWithProvenance(configPath, config.WithCredentials(stored.found))
	require.NoError(t, err)
	assert.Equal(t, ":9626", cfg.Server.Address)
	assert.Equal(t, config.OriginCredential, prov.Values["server.address"].Origin)

	_, _, err = config.LoadFromPathWithProvenance(configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADDR_BOOT_626")
}

func TestLogShadowedCredentials_NamesNeverValues_626(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	t.Setenv("SHADOW_626", "env-value")
	require.NoError(t, os.WriteFile(p, []byte("api:\n  auth_enabled: false\ndatabase:\n  driver: sqlite\n  path: "+filepath.Join(dir, "x.db")+"\nserver:\n  address: \":1\"\nchat:\n  api_key: \"${SHADOW_626}\"\n"), 0o600))
	cfg, _, err := config.LoadFromPathWithProvenance(p, config.WithCredentials(map[string]string{"SHADOW_626": "stored-value"}))
	require.NoError(t, err)

	var buf logBuffer
	c := &Container{Logger: zerolog.New(&buf)}
	c.logShadowedCredentials(cfg)
	assert.Contains(t, buf.String(), "credential SHADOW_626: the environment value shadows the stored one")
	assert.NotContains(t, buf.String(), "env-value")
	assert.NotContains(t, buf.String(), "stored-value")
}

type logBuffer struct{ b []byte }

func (l *logBuffer) Write(p []byte) (int, error) { l.b = append(l.b, p...); return len(p), nil }
func (l *logBuffer) String() string              { return string(l.b) }

// Review d2bc F4 (a3dc F4): a reload whose bootstrap pass fails says so.
func TestStageConfigForReload_AFailedBootstrapPassIsLogged_626(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	t.Setenv("NO_DB_626", "")
	require.NoError(t, os.WriteFile(p, []byte("api:\n  auth_enabled: false\ndatabase:\n  driver: sqlite\n  path: \"${NO_DB_626}\"\n"), 0o600))
	var buf logBuffer
	c := &Container{ConfigPath: p, Logger: zerolog.New(&buf)}
	_, _, err := c.stageConfigForReload()
	require.Error(t, err)
	assert.Contains(t, buf.String(), "the bootstrap pass failed")
}
