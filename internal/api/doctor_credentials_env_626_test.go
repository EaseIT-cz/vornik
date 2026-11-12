package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secretstore"
)

// GitHub #626 D: credentials_in_env_files.
func TestCheckCredentialsInEnvFiles_626(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("database:\n  password: \"${PG_PW_626}\"\nchat:\n  api_key: \"${FILE_ONLY_626}\"\nx:\n  a: \"${BOTH_626}\"\n  b: \"${SHADOW_626}\"\n"), 0o600))
	secrets := filepath.Join(dir, "secrets")
	require.NoError(t, os.MkdirAll(secrets, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "a.env"),
		[]byte("FILE_ONLY_626=f-secret\nBOTH_626=b-secret\nSHADOW_626=s-file\nPG_PW_626=pg-secret\nORPHAN_626=o\n"), 0o600))

	repo := &memAgentSecrets{}
	keyPath := filepath.Join(secrets, "store.key")
	key, err := secretstore.LoadOrCreateKey(keyPath)
	require.NoError(t, err)
	st, err := secretstore.New(repo, key)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, st.PutOperatorCredential(ctx, "BOTH_626", []byte("b-secret"), "t"))
	require.NoError(t, st.PutOperatorCredential(ctx, "SHADOW_626", []byte("s-stored"), "t"))
	t.Setenv("SHADOW_626", "s-live")
	t.Setenv("BOTH_626", "b-secret")

	h := &DoctorHandlers{}
	h.SetConfigPath(cfg)
	h.SetAgentSecretStore(repo, keyPath)
	got := h.checkCredentialsInEnvFiles(ctx)

	assert.Equal(t, "credentials_in_env_files", got.Name)
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "examined 5 name(s) in 1 env file(s); 3 secret(s) still in a file")
	all := strings.Join(got.Items, "\n")
	assert.Contains(t, all, "FILE_ONLY_626 ("+filepath.Join(secrets, "a.env")+":1): in an env file; move it with vornikctl credential import-env")
	assert.Contains(t, all, "BOTH_626 (")
	assert.Contains(t, all, "in both: the env file wins")
	assert.Contains(t, all, "SHADOW_626 (")
	assert.Contains(t, all, "shadowed")
	assert.Contains(t, all, "PG_PW_626 (")
	assert.Contains(t, all, "kept on disk by design")
	assert.Contains(t, all, "ORPHAN_626 (")
	assert.Contains(t, all, "unreferenced")
	for _, v := range []string{"f-secret", "b-secret", "s-file", "s-stored", "s-live", "pg-secret"} {
		assert.NotContains(t, all+got.Message, v, "never a value")
	}

	// Nothing left in a file: OK.
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "a.env"), []byte("PG_PW_626=pg-secret\n"), 0o600))
	got = h.checkCredentialsInEnvFiles(ctx)
	assert.Equal(t, "OK", got.Status)
	assert.Equal(t, "SKIPPED", (&DoctorHandlers{}).checkCredentialsInEnvFiles(ctx).Status)
}
