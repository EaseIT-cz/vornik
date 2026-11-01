package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #584 (operations-api-contract-design.md §6.6, review 9fd6 GREEN): one
// process-wide config-write lock. Before it, the session-era writers took no
// lock and could interleave with rotation, managed setup or a control-plane
// apply (a lost update, or a restore over another writer's change). Found
// at: review (the #582 design named them), observed at integration (two
// writers of one config.yaml); regression test at: integration, the writers
// on a real config file.

// waitsForTheLock runs write while the test holds configlock and requires
// the file unchanged until the lock is released, then changed.
func waitsForTheLock(t *testing.T, path string, write func() error) {
	t.Helper()
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	unlock := configlock.Lock()
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the writer finished while another held the config lock (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	during, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, during), "the file changed while another writer held the lock")
	unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the writer did not finish after the lock was released")
	}
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.False(t, bytes.Equal(before, after), "the writer wrote nothing")
}

func TestConfigLock584_EveryWriterWaitsForTheLock(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "chat.env")
	t.Run("the setup session commit", func(t *testing.T) {
		path := writeConfig582(t, managedYAML582())
		waitsForTheLock(t, path, func() error {
			return writeChatConfig(path, secret, onboarding.ChatConfigProposal{Endpoint: "https://llm.example.com/v1", Model: "m"})
		})
	})
	t.Run("the dispatcher commit", func(t *testing.T) {
		path := writeConfig582(t, managedYAML582())
		waitsForTheLock(t, path, func() error { return writeDispatcherProjectConfig(path, "assistant") })
	})
	t.Run("the memory commit", func(t *testing.T) {
		path := writeConfig582(t, managedYAML582())
		waitsForTheLock(t, path, func() error {
			return writeMemoryConfig(path, secret, onboarding.MemoryConfigProposal{Enabled: false})
		})
	})
}

// Review 9fd6 F1: managed setup's issuer and gateway probe makes network
// calls, so it runs outside the config lock. A slow provider must not hold
// up every config writer. The probe's stand-in checks that the lock is free
// while it runs.
func TestSetupManaged584_ProbesOutsideTheLock(t *testing.T) {
	body := strings.Replace(managedYAML582(), "server:\n", "server:\n  public_base_url: https://acme.vornik.cloud\n", 1)
	path := writeConfig582(t, body)
	cfg, err := config.LoadFromPath(path)
	require.NoError(t, err)
	probed, lockFree := 0, 0
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithSetupConfigPath(path),
		WithSetupSecretsDir(filepath.Join(t.TempDir(), "secrets")),
		WithSetupValidator(&gatewayLister582{models: []string{"open-a", "open-b"}}),
		WithAdminClassKeys(cfg.Admin),
		WithIssuerProbe(func(context.Context, string) error {
			probed++
			if release, ok := configlock.TryLock(); ok {
				lockFree++
				release()
			}
			return nil
		}),
	)
	hs := httptest.NewServer(NewRouter(srv, cfg).Handler())
	t.Cleanup(hs.Close)
	rig := &rigSetup582{path: path, hs: hs}
	status, out := rig.post(t, managedSetupBody582("sub-584"))
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, 1, probed)
	require.Equal(t, 1, lockFree, "the probe ran while the config lock was held")
}
