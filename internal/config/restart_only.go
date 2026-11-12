package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Restart-only config fields: values the running process reads ONCE and cannot pick up
// on a reload.
//
// INCIDENT 2026-07-30, customer deployment. Their memory classifier was hammering a
// shared LLM gateway every 30 seconds with calls that each took longer than that, causing
// congestion collapse — ~500 timeouts an hour and a stalled ingestion pipeline. The fix
// was to raise memory.classifier.auto_backfill_interval_seconds. `vornikctl config reload`
// logged "config reloaded successfully" TWICE and the worker kept its old 30-second
// ticker, because the backfill loop does `time.NewTicker(interval)` once at loop start and
// never re-reads the value. It took a restart, and nothing in the product said so.
//
// A reload that reports success on a value it cannot apply is worse than one that fails:
// the operator believes the change is live and goes looking elsewhere for the problem.
// That is the same shape as the systemd EnvironmentFile trap the troubleshooting guide
// already warns about, except that trap at least has documentation.
//
// THE LIST BELOW IS NOT EXHAUSTIVE, and saying so is the point. It holds the fields
// verified by reading the code that consumes them. Anything else that turns out to be
// read-once belongs here too — the correct reaction to finding one is to add it, not to
// assume this list was complete. A missing entry degrades to today's behaviour (silence),
// never to a false warning.

// restartOnlyPath is one field this daemon cannot re-read at runtime, with the reason.
type restartOnlyPath struct {
	path   string
	reason string
	value  func(*Config) string
}

// restartOnlyPaths enumerates the verified read-once fields.
var restartOnlyPaths = []restartOnlyPath{
	{
		path: "memory.classifier.auto_backfill_interval_seconds",
		reason: "the classify-backfill loop creates its ticker once at start " +
			"(internal/memory/classify_backfill.go: time.NewTicker(interval))",
		value: func(c *Config) string { return itoa(c.Memory.Classifier.AutoBackfillIntervalSeconds) },
	},
	{
		path:   "memory.classifier.auto_backfill_batch_size",
		reason: "passed to the classify-backfill loop as a parameter at start",
		value:  func(c *Config) string { return itoa(c.Memory.Classifier.AutoBackfillBatchSize) },
	},
	{
		path: "memory.titler.auto_backfill_interval_seconds",
		reason: "the title-backfill loop creates its ticker once at start " +
			"(internal/service/subsystem_memory_ingest.go passes s.titleInterval)",
		value: func(c *Config) string { return itoa(c.Memory.Titler.AutoBackfillIntervalSeconds) },
	},
	{
		path:   "memory.titler.auto_backfill_batch_size",
		reason: "passed to the title-backfill loop as a parameter at start",
		value:  func(c *Config) string { return itoa(c.Memory.Titler.AutoBackfillBatchSize) },
	},
	{
		path: "memory.provenance.forges",
		reason: "the automatic provenance clone manager is built once at start " +
			"(internal/service/memory_provenance.go provenanceAutoClones; GitHub #508)",
		value: provenanceForgesSummary,
	},
	{
		path:   "memory.provenance.replace_default_hosts",
		reason: "read with memory.provenance.forges when the clone manager is built at start",
		value:  func(c *Config) string { return strconv.FormatBool(c.Memory.Provenance.ReplaceDefaultHosts) },
	},
	// The Telegram bot is constructed once at start (internal/service/
	// container_subsystems.go initTelegram) and a reload never rebuilds it —
	// the same boot-time shape as the Slack channels of GitHub #738. Every
	// field of the block is listed so an edit to ANY of them is named; the
	// token itself is reported only as set/unset (a rotated token needs a
	// restart too, and its value must not land in a warning).
	{
		path:   "telegram.enabled",
		reason: "the Telegram bot is built once at start (initTelegram); a reload never rebuilds it",
		value:  func(c *Config) string { return strconv.FormatBool(c.Telegram.Enabled) },
	},
	{
		path:   "telegram.bot_token",
		reason: "passed to the bot constructor at start; a rotated token needs a restart",
		value:  func(c *Config) string { return secretSetOrUnset(c.Telegram.BotToken) },
	},
	{
		path:   "telegram.allowed_users",
		reason: "copied into the bot's admission map at start",
		value:  func(c *Config) string { return OneLineYAML(c.Telegram.AllowedUsers) },
	},
	{
		path:   "telegram.allow_unlisted_users",
		reason: "copied into the bot's admission rules at start",
		value:  func(c *Config) string { return strconv.FormatBool(c.Telegram.AllowUnlistedUsers) },
	},
	{
		path:   "telegram.rate_limit",
		reason: "the per-user limiter is created at start",
		value:  func(c *Config) string { return itoa(c.Telegram.RateLimit) },
	},
	{
		path:   "telegram.session_path",
		reason: "the session store is opened at start",
		value:  func(c *Config) string { return c.Telegram.SessionPath },
	},
	{
		path:   "telegram.session_ttl",
		reason: "parsed once at start",
		value:  func(c *Config) string { return c.Telegram.SessionTTL },
	},
	{
		path:   "telegram.dispatcher_project_id",
		reason: "pinned onto the dispatcher wiring at start",
		value:  func(c *Config) string { return c.Telegram.DispatcherProjectID },
	},
	{
		path:   "telegram.dispatcher_max_iterations",
		reason: "passed to the dispatcher loop construction at start",
		value:  func(c *Config) string { return itoa(c.Telegram.DispatcherMaxIterations) },
	},
	{
		path:   "telegram.forum_chat_id",
		reason: "forum routing is wired at start",
		value:  func(c *Config) string { return strconv.FormatInt(c.Telegram.ForumChatID, 10) },
	},
	{
		path:   "telegram.forum_topic_icon_color",
		reason: "read when forum topics are created from the start-time config",
		value:  func(c *Config) string { return itoa(c.Telegram.ForumTopicIconColor) },
	},
	{
		path:   "telegram.web_ui_base_url",
		reason: "rendered into onboarding links from the start-time config",
		value:  func(c *Config) string { return c.Telegram.WebUIBaseURL },
	},
}

// secretSetOrUnset reports whether a credential-shaped value is present without
// echoing it: a restart warning names values, and a token in journald or in
// `vornikctl config reload-status` output would outlive the warning's purpose.
func secretSetOrUnset(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unset"
	}
	return "set"
}

// OneLineYAML renders a value as deterministic single-line YAML for embedding in
// a restart-warning line. yaml.v3 sorts map keys, so the rendering is stable
// across snapshots and a diff means a real change.
func OneLineYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable: %v>", err)
	}
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// provenanceForgesSummary names each forge's shape for the restart warning:
// never a token value, only whether one is set (a rotated token is not
// detected here; it too needs a restart).
func provenanceForgesSummary(c *Config) string {
	var parts []string
	for _, f := range c.Memory.Provenance.Forges {
		cred := "anonymous"
		if f.Token != "" || f.TokenFile != "" {
			cred = "token"
		}
		parts = append(parts, fmt.Sprintf("%s(%s %s %s %s depth=%d projects=%s)", f.Name, f.Kind,
			f.ScopePrefix, f.BaseURL, cred, f.MaxRepoDepth, strings.Join(f.AllowedProjects, ",")))
	}
	return strings.Join(parts, "; ")
}

func itoa(i int) string { return strconv.Itoa(i) }

// RestartOnlySnapshotOf reads the restart-only fields out of cfg. Nil cfg yields nil so
// the probe can be called from a partially-built daemon without crashing the reload.
func RestartOnlySnapshotOf(cfg *Config) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(restartOnlyPaths))
	for _, f := range restartOnlyPaths {
		out[f.path] = f.value(cfg)
	}
	return out
}

// RestartOnlyChange is one field whose on-disk value no longer matches what the running
// process is using.
type RestartOnlyChange struct {
	Path string
	Was  string
	Now  string
}

// DiffRestartOnly reports the restart-only fields that differ between the values the
// process started with and the values now on disk. A key present on only one side counts
// as a change: unset-to-set is just as unapplied as one number to another.
func DiffRestartOnly(was, now map[string]string) []RestartOnlyChange {
	seen := make(map[string]struct{}, len(was)+len(now))
	for k := range was {
		seen[k] = struct{}{}
	}
	for k := range now {
		seen[k] = struct{}{}
	}
	var out []RestartOnlyChange
	for k := range seen {
		if was[k] != now[k] {
			out = append(out, RestartOnlyChange{Path: k, Was: was[k], Now: now[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// RestartOnlyWarning renders the operator-facing message. Empty for no changes so the
// caller can skip recording anything.
//
// It names the field, both values, and the reason — because the failure it prevents is an
// operator concluding their edit is live. "Requires a restart" alone would leave them
// wondering which value is actually running.
func RestartOnlyWarning(changed []RestartOnlyChange) string {
	if len(changed) == 0 {
		return ""
	}
	reasons := make(map[string]string, len(restartOnlyPaths))
	for _, f := range restartOnlyPaths {
		reasons[f.path] = f.reason
	}

	var b strings.Builder
	b.WriteString("reload applied, but ")
	if len(changed) == 1 {
		b.WriteString("1 setting cannot take effect without a daemon RESTART")
	} else {
		fmt.Fprintf(&b, "%d settings cannot take effect without a daemon RESTART", len(changed))
	}
	b.WriteString(" — the value below is on disk, the one in parentheses is what is still running:")
	for _, c := range changed {
		fmt.Fprintf(&b, "\n  %s = %s (running: %s)", c.Path, c.Now, c.Was)
		if r := reasons[c.Path]; r != "" {
			fmt.Fprintf(&b, " — %s", r)
		}
	}
	return b.String()
}

// SetRestartOnlyProbe wires a function returning the restart-only field values as they
// are on disk right now. Typically RestartOnlySnapshotOf over the live config.
//
// The baseline — the values the running workers were started with — is captured HERE,
// immediately, because this is called during container init while the config still is the
// one the workers are about to start with.
//
// Capturing it lazily on the first drift check instead would be silently wrong: the first
// check runs on the first RELOAD, by which time the file already holds the operator's
// edit, so the baseline would equal the new value and the very first change — the exact
// case this exists for — would produce no warning.
func (r *ConfigReloader) SetRestartOnlyProbe(f func() map[string]string) {
	if r == nil {
		return
	}
	var baseline map[string]string
	if f != nil {
		baseline = f()
	}
	r.mu.Lock()
	r.restartOnlyProbe = f
	if baseline != nil {
		r.restartOnlyBaseline = baseline
		r.restartOnlyBaselineSet = true
	}
	r.mu.Unlock()
}

// checkRestartOnlyDrift compares the on-disk restart-only values against the baseline and
// records a reload warning naming anything that cannot have taken effect.
//
// Reported through RecordReloadWarning, which already exists for exactly this class of
// outcome ("the reload succeeded but did not activate everything the operator dropped on
// disk") and is surfaced by `vornikctl config reload-status`. The success/failure contract
// is unchanged: the reload DID succeed, it just did not do everything the operator thinks.
//
// The baseline is NOT advanced when drift is found. The old value keeps running until a
// restart, so the warning has to persist across subsequent reloads rather than vanishing
// on the second one.
func (r *ConfigReloader) checkRestartOnlyDrift() {
	if r == nil {
		return
	}
	r.mu.Lock()
	probe := r.restartOnlyProbe
	baseline := r.restartOnlyBaseline
	haveBaseline := r.restartOnlyBaselineSet
	r.mu.Unlock()

	if probe == nil {
		return
	}
	current := probe()
	if current == nil {
		return
	}

	if !haveBaseline {
		r.mu.Lock()
		r.restartOnlyBaseline = current
		r.restartOnlyBaselineSet = true
		r.mu.Unlock()
		return
	}

	changed := DiffRestartOnly(baseline, current)
	if len(changed) == 0 {
		return
	}
	msg := RestartOnlyWarning(changed)
	r.RecordReloadWarning(msg)
	r.logger.Warn().
		Int("settings", len(changed)).
		Msg("config reload: some settings need a daemon restart to take effect")
}
