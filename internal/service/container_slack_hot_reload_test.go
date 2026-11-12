package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/slack"
)

// GitHub #738 (found at: production, operator observation 2026-11-11). A
// `config reload` reported success while the Slack allowlists kept their
// boot-time values: a channel the operator added stayed dropped, and — the
// security half — a sender the operator removed stayed admitted until
// restart. These are the acceptance tests: edit the allowlist, reload, and
// the running channel must reflect the edit (or the reload result must name
// the field as restart-required).
//
// Regression level: integration — a full NewContainer boot (initRegistry
// wires the real reloader; initHTTPServer builds the real Slack channel
// from the registry) driving a real reload cycle over files on disk.

const slackReloadSigningEnv = "TEST_SLACK_RELOAD_SIGNING_SECRET"

type slackReloadFixture struct {
	t             *testing.T
	c             *Container
	ch            *slack.Channel
	projectFile   string
	configFile    string
	signingSecret string
}

// newSlackReloadFixture boots a unit-test container whose registry holds one
// project with a fully configured slack block (team T123). The signing
// secret resolves through an env var the test owns; progress_signal is off
// so an admitted turn never tries to reach the Slack API.
func newSlackReloadFixture(t *testing.T, channelAllowlist, senderAllowlist []string) *slackReloadFixture {
	t.Helper()
	cfg := newComposerWiringTestConfig(t)
	cfg.Server.Address = "127.0.0.1:0" // Validate() requires it on the reload re-parse
	path := isolatedConfigPath(t)      // temp root + configs/{projects,swarms,workflows}
	dir := filepath.Dir(path)

	// A project must name a resolvable swarm AND workflow; these are the
	// minimal valid shapes (internal/registry/autonomy_mode_load_test.go).
	if err := os.WriteFile(filepath.Join(dir, "configs", "swarms", "reload-test-swarm.md"),
		[]byte("---\nswarmId: \"reload-test-swarm\"\nroles:\n  - name: \"worker\"\n    runtime:\n      image: \"x:latest\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "workflows", "reload-test-wf.md"),
		[]byte("---\nworkflowId: \"reload-test-wf\"\nentrypoint: \"step1\"\nsteps:\n  step1:\n    type: \"agent\"\n    role: \"worker\"\n    prompt: \"do work\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &slackReloadFixture{t: t, configFile: path, signingSecret: "shhh-reload-secret"}
	f.c = nil

	writeProject := func(channelAllowlist, senderAllowlist []string) {
		t.Helper()
		var b strings.Builder
		b.WriteString("projectId: reload-slack\n")
		b.WriteString("displayName: Reload Slack\n")
		b.WriteString("swarmId: reload-test-swarm\n")
		b.WriteString("defaultWorkflowId: reload-test-wf\n")
		b.WriteString("slack:\n")
		b.WriteString("  team_id: T123\n")
		b.WriteString("  signing_secret_env: " + slackReloadSigningEnv + "\n")
		if len(channelAllowlist) > 0 {
			b.WriteString("  channel_allowlist: [" + strings.Join(channelAllowlist, ", ") + "]\n")
		}
		if len(senderAllowlist) > 0 {
			b.WriteString("  sender_allowlist: [" + strings.Join(senderAllowlist, ", ") + "]\n")
		}
		b.WriteString("  progress_signal: false\n")
		if err := os.WriteFile(filepath.Join(dir, "configs", "projects", "reload-slack.yaml"),
			[]byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProject(channelAllowlist, senderAllowlist)
	f.projectFile = filepath.Join(dir, "configs", "projects", "reload-slack.yaml")

	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(slackReloadSigningEnv, f.signingSecret)
	c, err := newUnitTestContainer(t, cfg, path)
	if err != nil {
		t.Fatalf("newUnitTestContainer: %v", err)
	}
	f.c = c

	for _, ch := range c.SlackChannels {
		if ch.Name() == "" {
			continue
		}
		f.ch = ch // one project, one channel
		break
	}
	if f.ch == nil {
		t.Fatal("boot built no Slack channel from the project's slack block")
	}
	return f
}

// signedSlackEvent POSTs a signed message event to the channel's webhook
// handler, as Slack would deliver it.
func (f *slackReloadFixture) signedSlackEvent(channel, user, ts string) {
	f.t.Helper()
	body := fmt.Sprintf(`{"type":"event_callback","team_id":"T123","event_id":"Ev_%s",
		"event":{"type":"message","channel_type":"channel","user":%q,
		"channel":%q,"text":"@vornik hello","ts":%q}}`, ts, user, channel, ts)
	tsStr := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(f.signingSecret))
	_, _ = mac.Write([]byte("v0:" + tsStr + ":"))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/webhook", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	f.ch.HandleWebhook(httptest.NewRecorder(), req)
}

// sessionAppears polls ListSessions until a session under the given channel
// shows up (the inbound path records the session once every gate passes) or
// the deadline expires. Event dispatch is async inside the channel and the
// package-private wait seam is not reachable from here.
func (f *slackReloadFixture) sessionAppears(channel string, within time.Duration) bool {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		sessions, err := f.ch.ListSessions(context.Background())
		if err == nil {
			for _, s := range sessions {
				if strings.Contains(s.ID, "/"+channel+"#") {
					return true
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// setSessionAbsent lets the async dispatch settle, then reports whether any
// session under the channel exists. Used for the pre-reload refusal: the
// drop decision happens before any session recording, so a settled absence
// plus a later appearance from the SAME pipeline is the proof the gate —
// not the plumbing — was what blocked.
func (f *slackReloadFixture) setSessionAbsent(channel string, settle time.Duration) bool {
	f.t.Helper()
	time.Sleep(settle)
	sessions, err := f.ch.ListSessions(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range sessions {
		if strings.Contains(s.ID, "/"+channel+"#") {
			return true
		}
	}
	return false
}

// WIDENING (the reported incident): a channel added to channel_allowlist is
// admitted after a reload, without a restart.
func TestSlackAllowlistHotReload_WideningAdmitsTheNewChannel(t *testing.T) {
	f := newSlackReloadFixture(t, []string{"C_OLD"}, []string{"U_A"})

	// Precondition: the unlisted channel is dropped, the listed one is not.
	f.signedSlackEvent("C_PROBE", "U_A", "1.0")
	if f.setSessionAbsent("C_PROBE", 750*time.Millisecond) {
		t.Fatal("precondition: a channel outside channel_allowlist must be dropped")
	}

	// Operator adds the channel and reloads.
	content, err := os.ReadFile(f.projectFile)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "channel_allowlist: [C_OLD]",
		"channel_allowlist: [C_OLD, C_NEW]", 1))
	if err := os.WriteFile(f.projectFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	f.signedSlackEvent("C_NEW", "U_A", "2.0")
	if !f.sessionAppears("C_NEW", 3*time.Second) {
		t.Fatal("after the reload, a message from the newly allow-listed channel must be " +
			"admitted (a session is recorded) — the #738 incident is this failing")
	}
}

// NARROWING (the security half): a sender removed from sender_allowlist is
// refused after a reload, without a restart.
func TestSlackAllowlistHotReload_NarrowingRefusesTheRemovedSender(t *testing.T) {
	f := newSlackReloadFixture(t, []string{"C_A"}, []string{"U_A", "U_B"})
	ctx := context.Background()
	if _, err := f.ch.ResolveSpeaker(ctx, "U_B"); err != nil {
		t.Fatalf("precondition: U_B must be admitted before the edit: %v", err)
	}

	content, err := os.ReadFile(f.projectFile)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "sender_allowlist: [U_A, U_B]",
		"sender_allowlist: [U_A]", 1))
	if err := os.WriteFile(f.projectFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, err := f.ch.ResolveSpeaker(ctx, "U_B"); err == nil {
		t.Fatal("after the reload, the REMOVED sender must be refused — a revocation " +
			"that silently fails is a real exposure (#738)")
	} else if !strings.Contains(err.Error(), conversation.ErrSpeakerUnknown.Error()) && err != conversation.ErrSpeakerUnknown {
		t.Fatalf("refusal should be ErrSpeakerUnknown, got %v", err)
	}
	if _, err := f.ch.ResolveSpeaker(ctx, "U_A"); err != nil {
		t.Errorf("the still-listed sender U_A must stay admitted: %v", err)
	}
}

// Dead-probe regression (as-built record, 17-config-and-hot-reload.md): the
// restart-only drift probe was wired to c.Config, which a reload never
// updates, so NO restart-only warning could ever fire on a running daemon —
// including the memory.classifier.auto_backfill_interval_seconds field of
// the original 2026-07-30 incident. A changed config.yaml field must be
// named in the reload's warnings.
func TestRestartOnlyDrift_ChangedConfigYAMLFieldIsNamedInTheReload(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	cfg.Server.Address = "127.0.0.1:0" // Validate() requires it on the reload re-parse
	path := isolatedConfigPath(t)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newUnitTestContainer(t, cfg, path)
	if err != nil {
		t.Fatalf("newUnitTestContainer: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The composer test config leaves the field at its zero value, so the
	// marshalled file carries `auto_backfill_interval_seconds: 0` (classifier
	// and titler blocks alike); the first occurrence is a restart-only field
	// either way.
	edited := strings.Replace(string(content),
		"auto_backfill_interval_seconds: 0",
		"auto_backfill_interval_seconds: 600", 1)
	if edited == string(content) {
		t.Fatal("the marshalled config did not carry the backfill interval at its zero value")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	st := c.ConfigReloader.Status()
	if !st.HasWarnings {
		t.Fatal("a changed restart-only config.yaml field must raise a reload warning — " +
			"before the fix the probe compared c.Config against itself and could not")
	}
	joined := strings.Join(st.Warnings, " | ")
	if !strings.Contains(joined, "auto_backfill_interval_seconds") {
		t.Errorf("warning must name the field, got: %s", joined)
	}
}

// Denominator (AGENTS.md §4): the report must cover every field the reload
// examined, not only the ones someone thought to check. A change to the
// restart-only part of a project's channel block (here: slash_command) must
// be named as restart-required, keyed by project.
func TestRestartOnlyDrift_ProjectChannelBlockChangeIsNamedInTheReload(t *testing.T) {
	f := newSlackReloadFixture(t, []string{"C_A"}, []string{"U_A"})

	content, err := os.ReadFile(f.projectFile)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte("  slash_command: \"/helper\"\n")...)
	if err := os.WriteFile(f.projectFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	st := f.c.ConfigReloader.Status()
	if !st.HasWarnings {
		t.Fatal("a changed restart-only project-file channel setting must raise a reload " +
			"warning — the reload examined the whole block, so it must say so")
	}
	joined := strings.Join(st.Warnings, " | ")
	if !strings.Contains(joined, "project:reload-slack.slack") {
		t.Errorf("warning must name the project and its channel block, got: %s", joined)
	}
}

// Fail closed (PR 747 review, 2026-11-12): a reload that takes a project's
// Slack away cannot rebuild the running channel without a restart, but it
// must not leave the boot-time allowlists admitting senders either. Each
// case was admitted before the edit and must be refused after it.
func TestSlackAllowlistHotReload_TakingSlackAwayRefusesEverySender(t *testing.T) {
	cases := map[string]func(t *testing.T, f *slackReloadFixture){
		"slack block removed": func(t *testing.T, f *slackReloadFixture) {
			content, err := os.ReadFile(f.projectFile)
			if err != nil {
				t.Fatal(err)
			}
			body, _, found := strings.Cut(string(content), "slack:\n")
			if !found {
				t.Fatal("fixture has no slack block")
			}
			if err := os.WriteFile(f.projectFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"team_id changed": func(t *testing.T, f *slackReloadFixture) {
			content, err := os.ReadFile(f.projectFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.projectFile, []byte(strings.Replace(string(content), "team_id: T123", "team_id: T999", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"project removed": func(t *testing.T, f *slackReloadFixture) {
			if err := os.Remove(f.projectFile); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSlackReloadFixture(t, []string{"C_A"}, []string{"U_A"})
			ctx := context.Background()
			if _, err := f.ch.ResolveSpeaker(ctx, "U_A"); err != nil {
				t.Fatalf("precondition: U_A must be admitted before the edit: %v", err)
			}
			edit(t, f)
			_ = f.c.ConfigReloader.Reload() // a removed project may also be reported by the reload itself
			if _, err := f.ch.ResolveSpeaker(ctx, "U_A"); err == nil {
				t.Fatal("after the reload took Slack away, the boot-time sender must be refused until restart")
			}
		})
	}
}
