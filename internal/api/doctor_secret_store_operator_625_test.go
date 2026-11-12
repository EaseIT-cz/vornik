package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// GitHub #625 item 4 (agent-administered design, amendment #623, follow-up
// 2026-11-11). Found at: review (#623's "Not in this change"); a user observes
// it at production: with the store key missing, every operator credential
// silently resolves from the environment only, so MCP servers that need one
// stay unregistered, and the check spoke only of "agent credentials". It now
// counts operator credentials apart and names what a missing key masks.
func TestCheckSecretStoreKey_NamesOperatorCredentials_625(t *testing.T) {
	keyA := bytes.Repeat([]byte{1}, 32)
	repo := &memAgentSecrets{}
	st, err := secretstore.New(repo, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutOperatorCredential(context.Background(), "CONTEXT7_API_KEY", []byte("VALUE-CANARY"), "console"); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), "hermes", "FIO", "secret", []byte("VALUE-CANARY"), "dev"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		key     []byte
		status  string
		mention []string
	}{
		"key opens them": {keyA, "OK", []string{"1 agent credential(s) and 1 operator credential(s)"}},
		"key missing":    {nil, "ERROR", []string{"1 operator credential(s)", "resolve from the environment only"}},
		"wrong key":      {bytes.Repeat([]byte{2}, 32), "ERROR", []string{"does not open", "1 operator credential(s)", "resolve from the environment only"}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.key")
			if c.key != nil {
				if err := os.WriteFile(path, c.key, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h := &DoctorHandlers{}
			h.SetAgentSecretStore(repo, path)
			got := h.checkSecretStoreKey(context.Background(), false)
			if got.Status != c.status {
				t.Fatalf("status %s %q, want %s", got.Status, got.Message, c.status)
			}
			for _, m := range c.mention {
				if !strings.Contains(got.Message, m) {
					t.Errorf("message %q lacks %q", got.Message, m)
				}
			}
			if strings.Contains(got.Message, "VALUE-CANARY") {
				t.Fatal("the check leaked a value")
			}
		})
	}
}

// opsListFails lists nothing for the operator namespace, with an error.
type opsListFails struct{ *memAgentSecrets }

func (o opsListFails) List(ctx context.Context, ns string) ([]persistence.AgentSecretRow, error) {
	if ns == secretstore.OperatorCredentialsNamespace {
		return nil, errors.New("list failed")
	}
	return o.memAgentSecrets.List(ctx, ns)
}

// Review 1bae F1: a failed operator count is said, not miscounted.
func TestCheckSecretStoreKey_OperatorCountFailureIsSaid_625(t *testing.T) {
	repo := &memAgentSecrets{}
	st, _ := secretstore.New(repo, bytes.Repeat([]byte{1}, 32))
	if err := st.Put(context.Background(), "hermes", "FIO", "secret", []byte("v"), "dev"); err != nil {
		t.Fatal(err)
	}
	h := &DoctorHandlers{}
	h.SetAgentSecretStore(opsListFails{repo}, filepath.Join(t.TempDir(), "absent.key"))
	got := h.checkSecretStoreKey(context.Background(), false)
	if !strings.Contains(got.Message, "operator credentials could not be counted") {
		t.Fatalf("message %q", got.Message)
	}
}
