package service

import (
	"fmt"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence/subjectstore"
	"vornik.io/vornik/internal/secrets"
)

// initToolAuditRedaction wraps every repository the redaction seam covers so
// each writer's rows are scanned before they are persisted: tool_audit_log, and
// the content stores — chat_audit_log with chat_system_prompts, step_prompts,
// task_messages and execution_step_outcomes (its hallucination_signals).
//
// MUST run after the repos exist and BEFORE any consumer captures them — the
// same ordering constraint initLogship documents for its audit decorators. A
// consumer that grabbed an undecorated repo first would write raw rows and
// nothing would say so.
//
// Why a decorator at all: three writers reach tool_audit_log (the realtime
// POST handler, the post-step batch persist, the companion roll-up) and only
// the batch one scanned. Because the repository INSERT is
// ON CONFLICT (id) DO NOTHING and the two agent-side writers share the
// agent-supplied audit_id, the UNREDACTED realtime row landed first and won,
// silently discarding the redacted one. See
// https://docs.vornik.io
//
// EVERY decorator is installed in EVERY branch — a bypass carries a reason and
// counts each write — because until 2026-10-16 only the tool-audit one was: the
// content-store decorators were simply absent when scanning was off or the
// detector failed, so nothing counted the rows they let through, and
// task_messages had no decorator at all (chat-audit retention and redaction
// design §9.2.1, GitHub #395).
func (c *Container) initToolAuditRedaction() {
	if c.repos == nil {
		return
	}
	// The census holder is created ONCE and shared by every decorator instance.
	// On postgres there are two live ones for the life of the daemon: the
	// scheduler and dispatcher capture the decorators built at boot, and the
	// observability rebuild builds another set that only the re-run
	// initHTTPServer picks up. Per-instance counters would cover roughly half
	// the rows while presenting as complete. See the auditredact.Metrics doc.
	if c.auditRedactMetrics == nil {
		c.auditRedactMetrics = auditredact.NewMetrics()
	}
	// The Art 15 export builds its own default-pattern detector; when that
	// fails it withholds the column, and now says why in this log (§9.2.6).
	subjectstore.SetExportLogger(&c.Logger)

	reason := auditredact.ReasonNone
	var detector secrets.Detector
	var actions map[string]secrets.Action
	if !c.Config.Secrets.Enabled {
		// The operator disabled scanning outright; initScheduler already warns
		// loudly about that. The rows it lets through are still counted.
		reason = auditredact.ReasonSecretsDisabled
	} else {
		d, a, err := buildSecretsDetector(c.Config.Secrets)
		if err != nil || d == nil {
			c.Logger.Error().Err(err).Msg("secrets: redaction seams NOT scanning — detector failed to construct; tool_audit_log, chat_audit_log, chat_system_prompts, step_prompts, task_messages and execution_step_outcomes rows may persist plaintext credentials")
			reason = auditredact.ReasonDetectorUnavailable
		} else {
			detector, actions = d, a
		}
	}

	c.wireToolAuditSeam(reason, detector, actions)
	c.wireContentSeams(reason, detector)
	if reason == auditredact.ReasonNone {
		c.Logger.Info().
			Str("action", string(secrets.ResolveAction(secrets.CheckpointToolAudit, actions))).
			Msg("secrets: redaction wired at the repository seams — every writer is covered")
	}
}

// wireToolAuditSeam installs (or upgrades) the tool-audit decorator.
func (c *Container) wireToolAuditSeam(reason auditredact.Reason, detector secrets.Detector, actions map[string]secrets.Action) {
	if c.repos.ToolAudit == nil {
		return
	}
	base := c.repos.ToolAudit
	if existing, already := base.(*auditredact.Repo); already {
		// Idempotent: the observability rebuild re-runs this after replacing
		// the repo handles, and a double wrap would scan every row twice.
		//
		// A BYPASS wrapper is the exception — it must be upgradeable. A
		// pass-through pinned by the first call would keep the fleet writing
		// unscanned rows while the counter honestly reported a bypass the
		// second call should have fixed.
		if existing.BypassReason() == auditredact.ReasonNone {
			return
		}
		// Upgrading: wrap what the bypass wrapped, not the bypass itself.
		// Nesting would count each row twice. The replaced bypass is NOT
		// orphaned for counting purposes: consumers that captured it keep
		// writing through it, into the same shared holder.
		base = existing.Inner()
	}
	if reason != auditredact.ReasonNone {
		// A bypass is a decorator carrying a reason, never an absent decorator:
		// otherwise the object that would count the fail-open is never built
		// (Finding C, docs/audits/2026-08-26-silent-controls-audit.md).
		repo := auditredact.NewBypassed(base, reason, &c.Logger)
		repo.SetMetrics(c.auditRedactMetrics)
		c.repos.ToolAudit = repo
		return
	}
	wired := auditredact.New(base, detector, actions, c.repos.SecretRedaction, c.Config.Secrets.TrustedOutputTools, &c.Logger)
	wired.SetMetrics(c.auditRedactMetrics)
	c.repos.ToolAudit = wired
}

// seamDecorator is what the content-store decorators have in common for the
// idempotency and bypass-upgrade rule.
type seamDecorator interface{ BypassReason() auditredact.Reason }

// keepExisting reports whether an already-installed decorator stays: a
// scanning one always does (wrapping it again would scan twice), and a bypass
// does when it would be rebuilt with the same reason. Any other bypass is
// replaced by wrapping its Inner(), never nested.
func keepExisting(existing seamDecorator, reason auditredact.Reason) bool {
	r := existing.BypassReason()
	return r == auditredact.ReasonNone || r == reason
}

// wireContentSeams installs (or upgrades) the step-prompt, chat-audit and
// task-message decorators, in every branch (§9.2.1).
func (c *Container) wireContentSeams(reason auditredact.Reason, detector secrets.Detector) {
	c.wireStepPromptSeam(reason, detector)
	c.wireChatAuditSeam(reason, detector)
	c.wireTaskMessageSeam(reason, detector)
	c.wireStepOutcomeSeam(reason, detector)
}

// wireStepOutcomeSeam installs (or upgrades) the decorator over
// execution_step_outcomes (chat-audit retention and redaction design §11.2.1,
// GitHub #425). Only a repository that carries the optional methods callers
// type-assert for is wrapped: a decorator over anything narrower would either
// hide them or claim them. Both production repositories carry them; anything
// else stays undecorated, said here and reported by the doctor as detector_nil.
func (c *Container) wireStepOutcomeSeam(reason auditredact.Reason, detector secrets.Detector) {
	if c.repos.StepOutcomes == nil {
		return
	}
	var base auditredact.StepOutcomeStore
	switch existing := c.repos.StepOutcomes.(type) {
	case *auditredact.StepOutcomes:
		if keepExisting(existing, reason) {
			return
		}
		base = existing.Inner()
	case auditredact.StepOutcomeStore:
		base = existing
	default:
		c.Logger.Warn().Str("type", fmt.Sprintf("%T", c.repos.StepOutcomes)).
			Msg("secrets: execution_step_outcomes left undecorated — the repository lacks the optional sweep/stuck-step methods; hallucination_signals are persisted unscanned")
		return
	}
	var so *auditredact.StepOutcomes
	if reason == auditredact.ReasonNone {
		so = auditredact.NewStepOutcomes(base, detector, c.repos.SecretRedaction, &c.Logger)
	} else {
		so = auditredact.NewStepOutcomesBypassed(base, reason, &c.Logger)
	}
	so.SetMetrics(c.auditRedactMetrics)
	c.repos.StepOutcomes = so
}

func (c *Container) wireStepPromptSeam(reason auditredact.Reason, detector secrets.Detector) {
	base := c.repos.StepPrompts
	if base == nil {
		return
	}
	if existing, ok := base.(*auditredact.StepPrompts); ok {
		if keepExisting(existing, reason) {
			return
		}
		base = existing.Inner()
	}
	var sp *auditredact.StepPrompts
	if reason == auditredact.ReasonNone {
		sp = auditredact.NewStepPrompts(base, detector, c.repos.SecretRedaction, &c.Logger)
	} else {
		sp = auditredact.NewStepPromptsBypassed(base, reason, &c.Logger)
	}
	sp.SetMetrics(c.auditRedactMetrics)
	c.repos.StepPrompts = sp
}

func (c *Container) wireChatAuditSeam(reason auditredact.Reason, detector secrets.Detector) {
	base := c.repos.ChatAudit
	if base == nil {
		return
	}
	if existing, ok := base.(*auditredact.ChatAudit); ok {
		if keepExisting(existing, reason) {
			return
		}
		base = existing.Inner()
	}
	var ca *auditredact.ChatAudit
	if reason == auditredact.ReasonNone {
		ca = auditredact.NewChatAudit(base, detector, c.repos.SecretRedaction, &c.Logger)
	} else {
		ca = auditredact.NewChatAuditBypassed(base, reason, &c.Logger)
	}
	ca.SetMetrics(c.auditRedactMetrics)
	c.repos.ChatAudit = ca
}

func (c *Container) wireTaskMessageSeam(reason auditredact.Reason, detector secrets.Detector) {
	base := c.repos.Messages
	if base == nil {
		return
	}
	if existing, ok := base.(*auditredact.TaskMessages); ok {
		if keepExisting(existing, reason) {
			return
		}
		base = existing.Inner()
	}
	var tm *auditredact.TaskMessages
	if reason == auditredact.ReasonNone {
		tm = auditredact.NewTaskMessages(base, detector, c.repos.SecretRedaction, &c.Logger)
	} else {
		tm = auditredact.NewTaskMessagesBypassed(base, reason, &c.Logger)
	}
	tm.SetMetrics(c.auditRedactMetrics)
	c.repos.Messages = tm
}

// toolAuditRedactionState reports how the seam was wired, for the
// tool_audit_redaction doctor check: the bypass reason (empty when the seam is
// live) and the resolved action.
//
// It reads the DECORATOR, not the config. A config saying secrets.enabled=true
// is a statement of intent; whether rows are actually being scanned is a
// property of the object in the write path, and the gap between the two is the
// whole subject of the check.
func (c *Container) toolAuditRedactionState() (auditredact.Reason, string) {
	if c.repos == nil {
		return auditredact.ReasonDetectorNil, ""
	}
	repo, ok := c.repos.ToolAudit.(*auditredact.Repo)
	if !ok {
		return auditredact.ReasonDetectorNil, ""
	}
	if reason := repo.BypassReason(); reason != auditredact.ReasonNone {
		return reason, ""
	}
	return auditredact.ReasonNone, string(secrets.ResolveAction(secrets.CheckpointToolAudit, repo.Actions()))
}

// redactionSeamsState reports, for the redaction_seams doctor check, how the
// content-store decorators are wired: the worst reason any of them carries
// (ReasonNone when all of them scan), and the stores present. Like
// toolAuditRedactionState it reads the DECORATORS: a store present but not
// decorated reports detector_nil.
func (c *Container) redactionSeamsState() (auditredact.Reason, []string) {
	if c.repos == nil {
		return auditredact.ReasonDetectorNil, nil
	}
	type entry struct {
		store   string
		present bool
		dec     seamDecorator
	}
	entries := []entry{
		{auditredact.StoreChatAuditLog, c.repos.ChatAudit != nil, asSeam(c.repos.ChatAudit)},
		{auditredact.StoreStepPrompts, c.repos.StepPrompts != nil, asSeam(c.repos.StepPrompts)},
		{auditredact.StoreTaskMessages, c.repos.Messages != nil, asSeam(c.repos.Messages)},
		{auditredact.StoreExecutionStepOutcomes, c.repos.StepOutcomes != nil, asSeam(c.repos.StepOutcomes)},
	}
	worst := auditredact.ReasonNone
	var stores []string
	for _, e := range entries {
		if !e.present {
			continue
		}
		stores = append(stores, e.store)
		if e.store == auditredact.StoreChatAuditLog {
			stores = append(stores, auditredact.StoreChatSystemPrompts)
		}
		r := auditredact.ReasonDetectorNil
		if e.dec != nil {
			r = e.dec.BypassReason()
		}
		worst = worseReason(worst, r)
	}
	return worst, stores
}

func asSeam(v any) seamDecorator {
	if d, ok := v.(seamDecorator); ok {
		return d
	}
	return nil
}

// worseReason orders reasons by how much an operator needs to hear them:
// detector_unavailable (a control asked for is not running) over the two
// deployment choices, over none.
func worseReason(a, b auditredact.Reason) auditredact.Reason {
	rank := map[auditredact.Reason]int{
		auditredact.ReasonNone:                0,
		auditredact.ReasonDetectorNil:         1,
		auditredact.ReasonSecretsDisabled:     2,
		auditredact.ReasonDetectorUnavailable: 3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// exchangeRedactor is the recorder's seam (llm-exchange record/replay design
// §4): the same detector the step-prompt store was decorated with, exposed as
// a function so the API server never holds the decorator itself. Nil when the
// step-prompt decorator does not scan (no detector configured, or a bypass),
// in which case the recorder stores bodies as they are — the same position
// step prompts are in on such a deployment.
func (c *Container) exchangeRedactor() api.ExchangeRedactor {
	sp, ok := c.repos.StepPrompts.(*auditredact.StepPrompts)
	if !ok || !sp.Scans() {
		return nil
	}
	return func(body string) (string, int) {
		out, counts := sp.RedactText(body)
		n := 0
		for _, v := range counts {
			n += v
		}
		return out, n
	}
}
