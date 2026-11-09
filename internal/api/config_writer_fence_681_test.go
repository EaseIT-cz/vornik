package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #681 (operations-api-contract design §6.10, operator decision
// O-676-1): the setup commits refuse on a daemon that does not hold the
// config_writer lease, write nothing, and the handlers answer 409
// NOT_CONFIG_WRITER.
func TestConfigWriterFence681_SetupWritersRefuse(t *testing.T) {
	configlock.SetWriterCheck(func(context.Context) (bool, string) { return false, "lease held by node-b" })
	t.Cleanup(func() { configlock.SetWriterCheck(nil) })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	orig := []byte("server:\n  address: \":8080\"\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secrets", "chat.env")
	for name, call := range map[string]func() error{
		"chat": func() error {
			return writeChatConfig(context.Background(), path, secret, onboarding.ChatConfigProposal{Endpoint: "https://llm.example.com/v1", Model: "m"})
		},
		"dispatcher": func() error { return writeDispatcherProjectConfig(context.Background(), path, "assistant") },
		"memory": func() error {
			return writeMemoryConfig(context.Background(), path, secret, onboarding.MemoryConfigProposal{Enabled: false})
		},
	} {
		if err := call(); !errors.Is(err, configlock.ErrNotConfigWriter) {
			t.Fatalf("%s: err = %v, want ErrNotConfigWriter", name, err)
		}
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(orig) {
		t.Fatalf("a refused write changed the file:\n%s", got)
	}
	rr := httptest.NewRecorder()
	if !respondNotConfigWriter(rr, configlock.ErrNotConfigWriter) || rr.Code != http.StatusConflict {
		t.Fatalf("respondNotConfigWriter: code %d", rr.Code)
	}
	if respondNotConfigWriter(httptest.NewRecorder(), errors.New("other")) {
		t.Fatal("another error must not be answered as NOT_CONFIG_WRITER")
	}
}
