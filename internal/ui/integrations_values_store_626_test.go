package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/secretstore"
)

type storeOnlyUI626 map[string]string

func (m storeOnlyUI626) Get(name string) (string, bool) { v, ok := m[name]; return v, ok }

// GitHub #626 E: the hub's re-check reads a saved project secret held only in
// the credential store (the four reads in currentIntegrationValues).
func TestCurrentIntegrationValues_ReadTheStore_626(t *testing.T) {
	reg, _ := integrationsProjectRegistry(t)
	p := reg.GetProject("form-demo")
	if p == nil {
		t.Skip("fixture project missing")
	}
	p.Email.IMAPPasswordEnv, p.Email.SMTPPasswordEnv = "IMAP_UI_626", "SMTP_UI_626"
	p.Slack.BotTokenEnv, p.Slack.SigningSecretEnv = "BOT_UI_626", "SIGN_UI_626"
	secretstore.SetProcessSource(storeOnlyUI626{"IMAP_UI_626": "i", "SMTP_UI_626": "s", "BOT_UI_626": "b", "SIGN_UI_626": "g"})
	t.Cleanup(func() { secretstore.SetProcessSource(nil) })
	s := NewServer(WithProjectRegistry(reg))

	email := s.currentIntegrationValues("email", "form-demo")
	assert.Equal(t, "i", email["imap_password_env"])
	assert.Equal(t, "s", email["smtp_password_env"])
	sl := s.currentIntegrationValues("slack", "form-demo")
	assert.Equal(t, "b", sl["bot_token_env"])
	assert.Equal(t, "g", sl["signing_secret_env"])
}
