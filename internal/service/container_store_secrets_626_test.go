package service

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/slack"
)

// storeOnly626 resolves names from a map, standing in for a value held only
// in the operator credential store.
type storeOnly626 map[string]string

func (m storeOnly626) Get(name string) (string, bool) { v, ok := m[name]; return v, ok }

func useStore626(t *testing.T, m storeOnly626) {
	t.Helper()
	secretstore.SetProcessSource(m)
	t.Cleanup(func() { secretstore.SetProcessSource(nil) })
}

// GitHub #626 E: each point-of-use read uses a value held only in the store.
func TestChannelSecrets_AValueOnlyInTheStoreIsUsed_626(t *testing.T) {
	t.Run("email IMAP and SMTP passwords", func(t *testing.T) {
		useStore626(t, storeOnly626{"IMAP_626": "imap-stored", "SMTP_626": "smtp-stored"})
		p := buildEmailProject("test", "IMAP_626")
		p.Email.SMTPHost, p.Email.SMTPUsername, p.Email.FromAddress = "smtp.test", "u@test", "u@test"
		p.Email.SMTPPasswordEnv = "SMTP_626"
		ch, _, err := buildEmailChannel([]*registry.Project{p}, "", zerolog.Nop())
		if err != nil || ch == nil {
			t.Fatalf("buildEmailChannel with store-only passwords: ch=%v err=%v", ch, err)
		}
	})
	t.Run("slack signing secret", func(t *testing.T) {
		useStore626(t, storeOnly626{"SLACK_626": "signing-stored"})
		p := projectWithSlack("proj-a", "T_A", "SLACK_626", "")
		channels, _, err := buildSlackChannels([]*registry.Project{p}, slack.VoiceProviders{}, zerolog.Nop())
		if err != nil || len(channels) != 1 {
			t.Fatalf("buildSlackChannels with a store-only secret: %d channels, err=%v", len(channels), err)
		}
	})
	t.Run("github webhook secret", func(t *testing.T) {
		useStore626(t, storeOnly626{"GH_TEST_SECRET": "gh-stored"})
		ch, _, err := buildGitHubChannel([]*registry.Project{inboundOnlyProject("p-1")}, zerolog.Nop())
		if err != nil || ch == nil {
			t.Fatalf("buildGitHubChannel with a store-only secret: ch=%v err=%v", ch, err)
		}
	})
	t.Run("absent everywhere still fails", func(t *testing.T) {
		useStore626(t, storeOnly626{})
		if _, _, err := buildGitHubChannel([]*registry.Project{inboundOnlyProject("p-1")}, zerolog.Nop()); err == nil {
			t.Fatal("a secret in neither source must still fail the build")
		}
	})
}
