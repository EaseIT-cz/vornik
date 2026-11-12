package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secretstore"
)

type storeOnlyWebhook626 map[string]string

func (m storeOnlyWebhook626) Get(name string) (string, bool) { v, ok := m[name]; return v, ok }

// GitHub #626 E: a webhook source's secret held only in the credential store
// verifies a signature made with it.
func TestVerifyWebhookSignature_SecretFromTheStore_626(t *testing.T) {
	secretstore.SetProcessSource(storeOnlyWebhook626{"HOOK_626": "stored-hook-secret"})
	t.Cleanup(func() { secretstore.SetProcessSource(nil) })
	body := []byte(`{"event":"x"}`)
	mac := hmac.New(sha256.New, []byte("stored-hook-secret"))
	_, _ = mac.Write(body)
	r := httptest.NewRequest("POST", "/hooks/x", strings.NewReader(string(body)))
	r.Header.Set("X-Vornik-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	src := registry.ProjectWebhookSource{Name: "x", SecretEnv: "HOOK_626"}
	if err := verifyWebhookSignature(r, body, src); err != nil {
		t.Fatalf("store-only secret: %v", err)
	}
	src.SecretEnv = "HOOK_626_ABSENT"
	if err := verifyWebhookSignature(r, body, src); err == nil {
		t.Fatal("a secret in neither source must refuse")
	}
}
