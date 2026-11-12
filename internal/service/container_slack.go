package service

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/slack"
)

// buildSlackChannels scans the project registry for every project
// carrying a fully configured `slack` block and constructs one
// *slack.Channel per project. Returns aligned slices: channels[i]
// corresponds to projects[i]. Empty inputs (or no project with a
// slack block) return (nil, nil, nil) — the service container then
// leaves the inbound webhook handler unmounted entirely.
//
// Per-project routing: every enabled project gets its own channel
// instance pinned to its workspace (team_id). Each channel carries
// its own signing secret, bot token, and allowlists. The dispatcher
// receiver wired in container.go pins each channel to its project's
// ID so an inbound from project A can't accidentally invoke create_task
// against project B.
//
// Returned errors are operator-facing — empty signing-secret env var,
// duplicate team_id across projects, surface as daemon-boot failures
// so the misconfig is loud at startup rather than at the first
// delivery. A failure on one project's channel aborts the entire
// build so the operator sees the misconfig immediately; mirrors
// buildEmailChannels' posture.
//
// Multi-workspace deployments wire one project per workspace and one
// channel per project. The slack.Channel implementation itself
// supports multi-installation routing on a single channel (for an
// operator with a shared signing secret across workspaces), but the
// per-project YAML pattern keeps the boundary clean: project ⇔
// workspace ⇔ channel.
func buildSlackChannels(projects []*registry.Project, vp slack.VoiceProviders, logger zerolog.Logger) ([]*slack.Channel, []*registry.Project, error) {
	var (
		channels []*slack.Channel
		picked   []*registry.Project
	)
	seenTeams := make(map[string]string)
	for _, p := range projects {
		if !p.Slack.Enabled() {
			continue
		}
		if existing, ok := seenTeams[strings.TrimSpace(p.Slack.TeamID)]; ok {
			return nil, nil, fmt.Errorf("project %q slack: duplicate team_id %q (already configured by project %q)",
				p.ID, p.Slack.TeamID, existing)
		}
		ch, err := buildSlackChannelForProject(p, vp, logger)
		if err != nil {
			return nil, nil, err
		}
		seenTeams[strings.TrimSpace(p.Slack.TeamID)] = p.ID
		channels = append(channels, ch)
		picked = append(picked, p)
	}
	if len(channels) == 0 {
		return nil, nil, nil
	}
	return channels, picked, nil
}

// buildSlackChannelForProject is the per-project constructor
// buildSlackChannels delegates to. Kept separate for testability so
// callers (tests, future per-project hot-reload paths) can build a
// single channel without iterating a registry.
func buildSlackChannelForProject(p *registry.Project, vp slack.VoiceProviders, logger zerolog.Logger) (*slack.Channel, error) {
	cfg, err := resolveSlackConfig(p.Slack, p.ID)
	if err != nil {
		return nil, fmt.Errorf("project %q slack: %w", p.ID, err)
	}
	// Production HTTPClient dials through the shared SSRF guard (GitHub
	// #494, integrations-hub-design "Amendment 2026-10-21 (#494)"): it was
	// http.DefaultClient, and the channel also downloads file URLs a Slack
	// event names. Slack's API and file hosts are public, so no allowlist
	// entry applies. Timeout 0, as before: each request's context bounds it.
	// Tests inject via the channel's Config seam directly
	// (buildSlackChannelForProject is bypassed by tests that need a stub
	// Slack API server).
	cfg.Logger = channelLogger(logger, "slack", p.ID)
	cfg.HTTPClient = dialguard.DialGuard{}.HTTPClient(0)
	// Voice round-trip: the channel adapter is nil-safe per
	// direction, so a nil STT/TTS just keeps the pre-voice text-only
	// path active for that direction.
	// vp.SyntheticReplies is the resolved voice.synthetic_replies switch
	// (#543); with it off, replies to an audio clip are text.
	cfg.Voice = vp
	ch, err := slack.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("project %q slack: %w", p.ID, err)
	}
	return ch, nil
}

// resolveSlackConfig translates a registry.ProjectSlack YAML block
// into the slack.Config the channel constructor consumes. Env-var
// indirected secrets are resolved here; string allowlists pass
// through verbatim. The translation is one-project-one-installation
// — the resolved Config uses the Installations field (singular
// entry) rather than the top-level "single-installation back-compat"
// fields, so the channel's multi-installation routing primitive
// runs the same code path in single- and multi-project deployments.
func resolveSlackConfig(p registry.ProjectSlack, projectID string) (slack.Config, error) {
	signingSecret := secretstore.Value(p.SigningSecretEnv)
	if strings.TrimSpace(signingSecret) == "" {
		return slack.Config{}, fmt.Errorf("signing_secret_env %q is unset or empty", p.SigningSecretEnv)
	}
	// A turn takes 10-60s and Slack gives bots no typing indicator, so the placeholder
	// is ON unless the operator opts out. Zero here means "channel default": a negative
	// value is the only thing that disables the signal, so the opt-out has to be
	// explicit rather than an accident of the zero value.
	var progressDelay time.Duration
	if p.ProgressSignal != nil && !*p.ProgressSignal {
		progressDelay = -1
	} else if gap := strings.TrimSpace(p.ProgressSignalGap); gap != "" {
		parsed, err := time.ParseDuration(gap)
		if err != nil {
			return slack.Config{}, fmt.Errorf("progress_signal_gap %q: %w", gap, err)
		}
		progressDelay = parsed
	}

	cfg := slack.Config{
		SigningSecret:       signingSecret,
		TeamID:              strings.TrimSpace(p.TeamID),
		PostMessageRPS:      p.PostMessageRPS,
		PostMessageBurst:    p.PostMessageBurst,
		SlashCommand:        slack.NormaliseSlashCommand(p.SlashCommand),
		ProgressSignalDelay: progressDelay,
		Installations: []slack.InstallationConfig{
			{
				ProjectID:            projectID,
				TeamID:               strings.TrimSpace(p.TeamID),
				ChannelAllowlist:     p.ChannelAllowlist,
				SenderAllowlist:      p.SenderAllowlist,
				AllowUnlistedSenders: p.AllowUnlistedSenders,
			},
		},
	}
	if envName := strings.TrimSpace(p.BotTokenEnv); envName != "" {
		botToken := os.Getenv(envName)
		if strings.TrimSpace(botToken) == "" {
			// An operator who set BotTokenEnv expected outbound; an
			// empty resolved value is almost certainly a missing-env
			// misconfig. Fail loudly at boot — matches buildEmailChannel's
			// SMTP-password check.
			return slack.Config{}, fmt.Errorf("bot_token_env %q is unset or empty", envName)
		}
		cfg.Installations[0].BotToken = botToken
	}
	return cfg, nil
}

// applySlackHotConfig live-swaps the inbound allowlists on every running Slack
// channel (GitHub #738). The production incident: a reload reported success while
// channel_allowlist / sender_allowlist / allow_unlisted_senders kept their boot-time
// values — an added channel stayed dropped, and a REMOVED sender stayed admitted until
// restart. It runs as an activator step after the registry activates, so it reads the
// newly-activated project definitions; the swap itself is atomic inside the channel
// (slack.Channel.SetInstallationAllowlists), so webhook readers never see a torn set.
//
// Everything else in the slack block stays restart-only and is named by the
// restart-only probe via projectChannelRestartSnapshot. A channel whose project or
// workspace pin changed cannot be re-routed live (the HTTP route is pinned to the
// boot-time team_id), so those cases are recorded as reload warnings instead of
// guessed at.
func (c *Container) applySlackHotConfig(reloader *config.ConfigReloader) {
	if len(c.SlackChannels) == 0 || c.Registry == nil {
		return
	}
	activated := make(map[string]*registry.Project, len(c.Registry.ListProjects()))
	for _, p := range c.Registry.ListProjects() {
		if p != nil && p.ID != "" {
			activated[p.ID] = p
		}
	}
	warn := func(format string, args ...any) {
		c.Logger.Warn().Msgf("hot-reload: slack "+format, args...)
		if reloader != nil {
			reloader.RecordReloadWarning(fmt.Sprintf("slack: "+format, args...))
		}
	}
	for i, ch := range c.SlackChannels {
		if i >= len(c.SlackProjects) {
			// One project per channel is built together at boot; a channel
			// without one cannot be reconciled, so say so rather than skip it
			// silently (PR 747 review 9217 F3).
			warn("%d Slack channel(s) have no boot project to reconcile against; they keep their boot-time allowlists until restart", len(c.SlackChannels)-i)
			break
		}
		boot := c.SlackProjects[i]
		bootTeam := strings.TrimSpace(boot.Slack.TeamID)
		// Fail closed: when the reload takes a project's Slack away (the
		// project removed, its block disabled, or its workspace changed), the
		// running channel cannot be rebuilt without a restart, but it must not
		// keep admitting senders on the boot-time lists either. Its boot-time
		// installation is set to deny every channel and sender until restart.
		denyAll := func() {
			if _, err := ch.SetInstallationAllowlists(bootTeam, slack.InstallationConfig{ProjectID: boot.ID, TeamID: bootTeam}); err != nil {
				warn("project %q: could not close the boot-time installation (%v); restart to apply", boot.ID, err)
			}
		}
		p, ok := activated[boot.ID]
		if !ok {
			denyAll()
			warn("project %q was removed; its Slack channel now refuses every sender, and is removed at restart", boot.ID)
			continue
		}
		if !p.Slack.Enabled() {
			denyAll()
			warn("project %q slack block is disabled or incomplete; its Slack channel now refuses every sender until restart", p.ID)
			continue
		}
		team := strings.TrimSpace(p.Slack.TeamID)
		if team != bootTeam {
			denyAll()
			warn("project %q: team_id changed from %q to %q; the channel is routed by its boot-time workspace, so it now refuses every sender until a restart builds the new one", p.ID, bootTeam, team)
			continue
		}
		changed, err := ch.SetInstallationAllowlists(team, slack.InstallationConfig{
			ProjectID:            p.ID,
			TeamID:               team,
			ChannelAllowlist:     p.Slack.ChannelAllowlist,
			SenderAllowlist:      p.Slack.SenderAllowlist,
			AllowUnlistedSenders: p.Slack.AllowUnlistedSenders,
		})
		if err != nil {
			// The channel routes by its boot-time team_id, so a changed
			// workspace pin is restart-required, not swappable.
			warn("project %q: %v — the channel is routed by its boot-time workspace, so this needs a restart", p.ID, err)
			continue
		}
		if changed {
			c.Logger.Info().Str("project", p.ID).
				Msg("hot-reload: applied slack allowlists without restart")
		}
	}
}

// slackSlashCommands returns EVERY distinct slash command this daemon's Slack
// channels answer, sorted, for the UI panel that has to tell a person what to
// type.
//
// Every one, not the first. The first cut returned a single command and an
// operator running two Slack apps — /t800 and /helper — was told to use
// whichever project happened to load first. A review had flagged the
// single-command assumption as "likely fine (one workspace, one command) but
// unasserted"; it was neither fine nor, as it turned out, true.
//
// Read from the CHANNEL rather than re-derived from config, because
// NormaliseSlashCommand has already been applied there — re-normalising here
// would be a second implementation of the same rule, and the one that drifts
// is the one nobody exercises.
func (c *Container) slackSlashCommands() []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range c.SlackChannels {
		cmd := ch.SlashCommand()
		if cmd == "" || seen[cmd] {
			continue
		}
		seen[cmd] = true
		out = append(out, cmd)
	}
	sort.Strings(out)
	return out
}
