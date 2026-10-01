package service

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"vornik.io/vornik/internal/brokeractions"
	"vornik.io/vornik/internal/companionpush"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// newBrokerActionWorker builds the daemon's broker action worker (broker
// write-actions design §5.4), or nil when this node cannot execute actions:
// no broker-action store, or no MCP manager to call the write tool through.
//
// Built in Run, not in NewContainer: initHTTPServer runs twice and the
// Postgres path rebuilds c.repos in between, so only Run sees the final
// store. The /inbox approve handler reaches the worker through a closure
// that reads c.brokerActionWorker at call time; a nil worker's Kick is a
// no-op and the next sweep, on any node, picks the action up.
func (c *Container) newBrokerActionWorker() *brokeractions.Worker {
	if c.repos == nil || c.repos.BrokerActions == nil || c.mcpManager == nil {
		return nil
	}
	return brokeractions.New(c.brokerActionWorkerConfig())
}

// brokerActionWorkerConfig is the worker's wiring, split out for tests.
func (c *Container) brokerActionWorkerConfig() brokeractions.Config {
	// The loader refuses an invalid action_timeout; were one to reach here,
	// the zero it resolves to makes the worker use its 60s default.
	timeout, _ := c.Config.Broker.EffectiveActionTimeout()
	if c.brokerActionMetrics == nil {
		c.brokerActionMetrics = brokeractions.NewMetrics()
	}
	metrics := c.brokerActionMetrics
	redact, unscannedReason := c.brokerActionRedactor()
	return brokeractions.Config{
		Repo:   c.repos.BrokerActions,
		Caller: c.mcpManager,
		WritesOn: func() bool {
			mode, err := c.Config.Broker.WritesMode()
			return err == nil && mode == "on"
		},
		ToolDeclared: func(projectID, tool string) error {
			if c.Registry == nil {
				return errors.New("project registry unavailable")
			}
			p := c.Registry.GetProject(projectID)
			if p == nil {
				return errors.New("project is no longer loaded")
			}
			return registry.BrokerWriteToolDeclared(p, tool)
		},
		Timeout: timeout,
		Redact:  redact,
		OnUnscanned: func() {
			metrics.RecordUnscanned(unscannedReason)
		},
		OnFinishError: func(*persistence.BrokerAction, string) {
			metrics.RecordFinishFailed()
		},
		// Tell the push outbox (design §7a).
		OnChange: func() { c.companionPusher.Kick() },
		Gauge:    metrics.SetStuck,
		OnTransition: func(a *persistence.BrokerAction) {
			metrics.RecordFinished(a.OutcomeClass)
		},
		Logger: c.Logger.With().Str("component", "broker-actions").Logger(),
	}
}

// brokerActionRedactor scrubs a tool response before it is stored as the
// action's outcome, with the same detector the tool-audit seam uses.
// Always redacts on a finding: the outcome is shown to operators, and the
// detect-only choice for tool audit is about audit fidelity, which an
// outcome summary does not need. Without a detector it returns nil and the
// reason: the response is stored as returned, as tool-audit rows are, and
// each such outcome is counted in
// vornik_broker_actions_outcomes_unscanned_total (review-20260930-bd00 F5).
func (c *Container) brokerActionRedactor() (func([]byte) []byte, string) {
	if !c.Config.Secrets.Enabled {
		return nil, "secrets_disabled"
	}
	detector, _, err := buildSecretsDetector(c.Config.Secrets)
	if err != nil || detector == nil {
		c.Logger.Error().Err(err).Msg("broker actions: secrets detector failed to construct; outcomes are stored unredacted")
		return nil, "detector_unavailable"
	}
	return func(b []byte) []byte {
		if findings := detector.Scan(b); len(findings) > 0 {
			return secrets.Redact(b, findings)
		}
		return b
	}, ""
}

// notifyBrokerActionsPending sends the notify-only Telegram alert for writes
// promoted to pending (design §5.3). It reads c.TelegramBot at call time:
// the executor is built before the bot, but the bot is assigned once in
// NewContainer (initTelegram) and executor work only starts in Run, so the
// write happens before any read (review-20260930-8b18 F3). It sends in the
// background so the executor never waits on Telegram. No bot: nothing.
func (c *Container) notifyBrokerActionsPending(_ context.Context, projectID, taskID string, n int) {
	// Promotion is a transition the push outbox sends (design §7a).
	c.companionPusher.Kick()
	bot := c.TelegramBot
	if bot == nil {
		return
	}
	inbox := brokerInboxURL(c.Config.Server.PublicBaseURL)
	runAlert(c.Logger.With().Str("task_id", taskID).Logger(), "broker-actions", func(ctx context.Context) error {
		return bot.NotifyBrokerActionsPending(ctx, projectID, taskID, n, inbox)
	})
}

// brokerInboxURL is the /inbox deep link: absolute when the operator set a
// public base URL, a path otherwise.
func brokerInboxURL(publicBase string) string {
	if base := strings.TrimRight(strings.TrimSpace(publicBase), "/"); base != "" {
		return base + "/ui/inbox"
	}
	return "/ui/inbox"
}

// newCompanionPusher builds the companion push loop (broker write-actions
// design §7a), or nil without the outbox or the push-config store. Built in
// Run for the same reason as the action worker: only Run sees the final
// repositories. The UI, the worker and the Telegram hook reach it through
// closures that read c.companionPusher at call time; a nil pusher's Kick is
// a no-op.
func (c *Container) newCompanionPusher() *companionpush.Pusher {
	if !c.companionPushWired() {
		return nil
	}
	if c.companionPushMetrics == nil {
		c.companionPushMetrics = companionpush.NewMetrics()
	}
	metrics := c.companionPushMetrics
	return companionpush.New(companionpush.Config{
		Outbox: c.repos.CompanionPushOutbox,
		Allowed: func(projectID string) []netip.Prefix {
			if c.Registry == nil {
				return nil
			}
			return c.Registry.GetProject(projectID).CompanionPushAllowed()
		},
		OnResult: metrics.Record,
		Logger:   c.Logger.With().Str("component", "companion-push").Logger(),
	})
}

// companionPushWired is the one condition for push: the api advertises
// companion-push and accepts notify exactly when the pusher will run.
func (c *Container) companionPushWired() bool {
	return c.repos != nil && c.repos.CompanionPushOutbox != nil && c.repos.A2APushConfigs != nil
}
