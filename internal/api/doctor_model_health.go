package api

// Doctor check: model_health.
//
// For every model a swarm role pins (`model` + `modelFallback`), compute a
// recent runtime health signal and flag models that are failing or producing
// degenerate output. This is the check that would have caught the dead
// `z-ai/glm-4.5-air:free` (100% step failure) and the local `qwen3.6:35b`
// (timeouts + empty output) before they silently degraded every task routed
// through the affected role.
//
// Data source — execution_step_outcomes + task_llm_usage:
//
//   - execution_step_outcomes is the daemon's purpose-built per-(role,model)
//     output-quality table (its own column comment: "model-effectiveness
//     metrics reflect real output quality per (role, model)"). The `outcome`
//     taxonomy distinguishes 'ok' from parse_error / schema_violation /
//     refused / degenerate_loop / timeout / failed — exactly the failure
//     shapes a bad model produces. This is the failure-rate signal.
//   - task_llm_usage carries completion_tokens per call; a model that times
//     out or returns empty output shows a degenerate (near-zero) median
//     completion-token count even when the step is later salvaged. This is
//     the degenerate-output signal that catches a model which "fails quietly"
//     rather than erroring outright.
//
// We deliberately do NOT use the Prometheus registry: it's process-lifetime
// and resets on restart, whereas the DB tables give a stable recent window
// that survives a daemon bounce — the right horizon for an operator running
// `vornikctl doctor` after noticing trouble.
//
// RECOMMEND, never auto-switch: a flagged model's finding names the role's
// configured modelFallback (or notes none is set). There is no --fix mutation
// — swapping a model under a live trading swarm is too risky to automate.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"

	"vornik.io/vornik/internal/config"
)

const (
	// modelHealthWindow bounds how far back the recent-health query looks.
	modelHealthWindow = 24 * time.Hour
	// modelHealthRowCap bounds the per-model aggregation scan so a busy
	// deployment can't turn the check into a table sweep.
	modelHealthRowCap = 50000
	// The sample floor and the failure rate moved to `doctor.thresholds`
	// (2026-09-13) — config.DefaultModelMinSamples and
	// config.DefaultModelFailureRate are the same numbers this check shipped
	// with. They are shared with model_calls_live deliberately: it is ONE
	// judgement about what "a model is failing" means, and two copies of one
	// judgement is how the two checks would come to disagree.
	// modelHealthDegenerateTokens is the median completion-token count below
	// which a model's output is considered degenerate (empty/near-empty).
	modelHealthDegenerateTokens = 10
)

// modelHealthStat is one model's recent aggregate runtime health.
type modelHealthStat struct {
	model                  string
	samples                int   // recent steps CHARGED to this model (excluded classes removed)
	failures               int   // charged steps whose outcome was not 'ok'/'pending_validation'
	medianCompletionTokens int64 // median completion_tokens across recent calls
	// excludedByClass counts recent failures NOT charged to the model — the
	// container, host or an upstream step failed (stepoutcome.
	// NotAttributableToModel; model-health attribution design, 2026-09-24).
	excludedByClass map[string]int
}

// excludedTotal sums excludedByClass.
func (s modelHealthStat) excludedTotal() int {
	n := 0
	for _, c := range s.excludedByClass {
		n += c
	}
	return n
}

// dominantExcluded returns the excluded class with the most failures (ties by
// name, for a stable message).
func (s modelHealthStat) dominantExcluded() (string, int) {
	best, bestN := "", 0
	for c, n := range s.excludedByClass {
		if n > bestN || (n == bestN && c < best) {
			best, bestN = c, n
		}
	}
	return best, bestN
}

// excludedBreakdown renders "class: n, class: n", largest first.
func (s modelHealthStat) excludedBreakdown() string {
	classes := make([]string, 0, len(s.excludedByClass))
	for c := range s.excludedByClass {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		if s.excludedByClass[classes[i]] != s.excludedByClass[classes[j]] {
			return s.excludedByClass[classes[i]] > s.excludedByClass[classes[j]]
		}
		return classes[i] < classes[j]
	})
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s: %d", c, s.excludedByClass[c]))
	}
	return strings.Join(parts, ", ")
}

// excludedDominates is the host-problem rule: a model's recent failures that
// were NOT its own outnumber the ones that were, and are at least the sample
// floor. "Outnumber", not a ratio: the finding's claim is "most of what went
// wrong here was not the model", and a majority is what that sentence means.
// Not gated on the charged floor — in an ONGOING outage the newest rows (the
// row cap takes the newest) can all be excluded, and a check that went silent
// then would hide the model when it matters most.
func excludedDominates(s modelHealthStat, floor int) bool {
	ex := s.excludedTotal()
	return ex >= floor && ex > s.failures
}

// modelHealthFinding is one flagged model with its severity + recommendation.
type modelHealthFinding struct {
	model   string
	status  string // WARNING | ERROR
	message string
}

// SetModelHealthSource overrides the recent-health data source. Optional —
// when unset, checkModelHealth uses the DB-backed query (or skips if there's
// no DB). Tests inject a fake to exercise the check without a database.
func (h *DoctorHandlers) SetModelHealthSource(src func(ctx context.Context) ([]modelHealthStat, error)) {
	if h == nil {
		return
	}
	h.modelHealthSource = src
}

// SetChatProvider wires the live circuit-breaker reporter for
// checkModelCircuits. When the provider implements chat.ModelHealthReporter
// (the router with the health-gate layer enabled), the doctor surfaces a
// live per-(route, model) circuit line. A plain client or a disabled
// breaker layer leaves modelCircuits nil and the check skips.
func (h *DoctorHandlers) SetChatProvider(p chat.Provider) {
	if h == nil {
		return
	}
	if reporter, ok := p.(chat.ModelHealthReporter); ok {
		h.modelCircuits = reporter.ModelHealthSnapshot
	}
}

// SetAgentHealthReporter wires the live AGENT-container circuit-breaker reporter
// for checkAgentModelCircuits. The agenthealth.Registry implements
// chat.ModelHealthReporter; a nil reporter leaves agentCircuits nil and the
// check skips. (ModelHealthSnapshot is itself nil-safe, so a typed-nil registry
// is also fine.)
func (h *DoctorHandlers) SetAgentHealthReporter(r chat.ModelHealthReporter) {
	if h == nil || r == nil {
		return
	}
	h.agentCircuits = r.ModelHealthSnapshot
}

// checkModelCircuits surfaces the LIVE model-health circuit-breaker state
// alongside the 24h passive checkModelHealth. Unlike checkModelHealth (a DB
// history read that recommends a fallback), this reflects the in-memory
// breaker registry right now: any OPEN or HALF_OPEN circuit means the chat
// router is actively shedding a model. Read-only.
func (h *DoctorHandlers) checkModelCircuits() DoctorCheck {
	const name = "model_circuits"
	if h.modelCircuits == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "chat health-gate not wired; skipping live circuit check"}
	}
	snaps := h.modelCircuits()
	if len(snaps) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no model circuits initialized yet (no chat traffic since boot)"}
	}
	// Sort for stable output: open first, then half_open, then by route/model.
	sort.Slice(snaps, func(i, j int) bool {
		oi, oj := circuitSeverity(snaps[i].State), circuitSeverity(snaps[j].State)
		if oi != oj {
			return oi > oj
		}
		if snaps[i].Route != snaps[j].Route {
			return snaps[i].Route < snaps[j].Route
		}
		return snaps[i].Model < snaps[j].Model
	})
	var degraded []string
	worst := "OK"
	for _, s := range snaps {
		switch s.State {
		case "open":
			worst = "ERROR"
			degraded = append(degraded, fmt.Sprintf("[ERROR] %s/%s OPEN since %s — router shedding to fallback",
				s.Route, s.Model, s.OpenSince.Format(time.RFC3339)))
		case "half_open":
			if worst != "ERROR" {
				worst = "WARNING"
			}
			degraded = append(degraded, fmt.Sprintf("[WARNING] %s/%s HALF_OPEN — probing recovery", s.Route, s.Model))
		}
	}
	if len(degraded) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("all %d model circuit(s) closed", len(snaps))}
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d of %d model circuit(s) not closed — the chat router is actively degrading", len(degraded), len(snaps)),
		Items:   degraded,
	}
}

// checkAgentModelCircuits surfaces the LIVE agent-container circuit-breaker
// state — the breaker that gates agent-step LLM calls BEFORE the chat router, so
// an open agent circuit can leave the chat-router circuit CLOSED (the two-breaker
// gotcha, 2026-07-18). Distinct from checkModelCircuits (the chat-router
// breaker). Read-only.
func (h *DoctorHandlers) checkAgentModelCircuits() DoctorCheck {
	const name = "agent_model_circuits"
	if h.agentCircuits == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent health-gate not wired; skipping live agent circuit check"}
	}
	snaps := h.agentCircuits()
	if len(snaps) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no agent model circuits initialized yet (no agent LLM calls since boot)"}
	}
	// Sort worst-first, then by model (the agent breaker's route is always "agent").
	sort.Slice(snaps, func(i, j int) bool {
		oi, oj := circuitSeverity(snaps[i].State), circuitSeverity(snaps[j].State)
		if oi != oj {
			return oi > oj
		}
		return snaps[i].Model < snaps[j].Model
	})
	var degraded []string
	worst := "OK"
	for _, s := range snaps {
		switch s.State {
		case "open":
			worst = "ERROR"
			degraded = append(degraded, fmt.Sprintf("[ERROR] agent/%s OPEN since %s — agent steps failing over to role modelFallback",
				s.Model, s.OpenSince.Format(time.RFC3339)))
		case "half_open":
			if worst != "ERROR" {
				worst = "WARNING"
			}
			degraded = append(degraded, fmt.Sprintf("[WARNING] agent/%s HALF_OPEN — probing recovery", s.Model))
		}
	}
	if len(degraded) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("all %d agent model circuit(s) closed", len(snaps))}
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d of %d agent model circuit(s) not closed — the agent LLM path is actively degrading", len(degraded), len(snaps)),
		Items:   degraded,
	}
}

// circuitSeverity orders circuit states for stable worst-first output.
func circuitSeverity(state string) int {
	switch state {
	case "open":
		return 2
	case "half_open":
		return 1
	default:
		return 0
	}
}

// checkModelHealth flags swarm-role models with a poor recent health signal
// and RECOMMENDS each role's configured fallback. Read-only — no --fix.
func (h *DoctorHandlers) checkModelHealth(ctx context.Context) DoctorCheck {
	name := "model_health"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config dir; skipping"}
	}

	source := h.modelHealthSource
	if source == nil {
		if h.db == nil {
			return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no health data source wired; skipping"}
		}
		source = h.queryModelHealthStats
	}

	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v", err)}
	}

	referenced, fallbacks := collectModelFallbacks(reg.ListSwarms())
	if len(referenced) == 0 {
		// doctor-vacuous: OK is correct here, not SKIPPED. The check RAN —
		// collectModelFallbacks walked every swarm and found no role pins. An
		// empty set is a true pass, not an unevaluated one, and reporting it as
		// SKIPPED would hide a deployment that genuinely pins no models behind
		// the same label as one where the check could not run.
		return DoctorCheck{Name: name, Status: "OK", Message: "no role-pinned models to evaluate"}
	}

	stats, err := source(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("health query failed: %v", err)}
	}

	// Restrict to referenced models only — we don't alarm on models no role
	// uses (e.g. historical rows from a since-removed config).
	scoped := stats[:0:0]
	for _, s := range stats {
		if referenced[s.model] {
			scoped = append(scoped, s)
		}
	}

	findings := evalModelHealth(scoped, fallbacks, h.doctorThresholds())
	if len(findings) == 0 {
		// Scope named explicitly: see modelHealthHealthySummary for why the previous
		// wording misled an operator through a live outage on 2026-07-30.
		return DoctorCheck{Name: name, Status: "OK", Message: modelHealthHealthySummary(len(referenced))}
	}

	worst := "WARNING"
	items := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.status == "ERROR" {
			worst = "ERROR"
		}
		items = append(items, fmt.Sprintf("[%s] %s", f.status, f.message))
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d role-pinned model(s) degraded over last %s — review and consider the recommended fallback", len(findings), modelHealthWindow),
		Items:   items,
	}
}

// collectModelFallbacks returns the set of models referenced by any swarm role
// and a model→recommended-fallback map. A model can appear in several roles;
// the first non-empty fallback seen wins for the recommendation text. A model
// that appears only as a fallback is referenced (so it's evaluated) but has no
// fallback of its own modeled.
func collectModelFallbacks(swarms []*registry.Swarm) (referenced map[string]bool, fallbacks map[string]string) {
	referenced = map[string]bool{}
	fallbacks = map[string]string{}
	for _, s := range swarms {
		if s == nil {
			continue
		}
		for _, role := range s.Roles {
			if m := strings.TrimSpace(role.Model); m != "" {
				referenced[m] = true
				if _, ok := fallbacks[m]; !ok {
					fallbacks[m] = strings.TrimSpace(role.ModelFallback)
				}
			}
			if fb := strings.TrimSpace(role.ModelFallback); fb != "" {
				referenced[fb] = true
				if _, ok := fallbacks[fb]; !ok {
					fallbacks[fb] = ""
				}
			}
		}
	}
	return referenced, fallbacks
}

// evalModelHealth scores each stat and returns a finding per unhealthy model.
// Pure — directly unit-testable. fallbacks maps model → its recommended
// fallback ("" = none configured).
func evalModelHealth(stats []modelHealthStat, fallbacks map[string]string, th config.ResolvedDoctorThresholds) []modelHealthFinding {
	var findings []modelHealthFinding
	for _, s := range stats {
		// The host-problem finding is its own row, emitted whatever the
		// charged sample count (design: an ongoing outage leaves none).
		host := excludedDominates(s, th.ModelMinSamples.Value)
		var hostFinding *modelHealthFinding
		if host {
			class, _ := s.dominantExcluded()
			hostFinding = &modelHealthFinding{
				model:  s.model,
				status: "WARNING",
				message: fmt.Sprintf("%s: %d recent failure(s) NOT charged to the model (%s) outnumber its own (%d/%d) — "+
					"a container/host or upstream problem, not the model; check %s before changing models",
					s.model, s.excludedTotal(), s.excludedBreakdown(), s.failures, s.samples, hostCheckFor(class)),
			}
		}
		// The sample floor applies to the CHARGED denominator.
		if s.samples < th.ModelMinSamples.Value {
			if hostFinding != nil {
				findings = append(findings, *hostFinding)
			}
			continue
		}
		failRate := float64(s.failures) / float64(s.samples)
		degenerate := s.medianCompletionTokens < modelHealthDegenerateTokens
		highFail := failRate >= th.ModelFailureRate.Value
		if !highFail && !degenerate {
			if hostFinding != nil {
				findings = append(findings, *hostFinding)
			}
			continue
		}

		var reasons []string
		status := "WARNING"
		if highFail {
			reasons = append(reasons, fmt.Sprintf("%.0f%% step-failure rate (%d/%d)", failRate*100, s.failures, s.samples))
			if failRate >= 0.9 {
				status = "ERROR"
			}
		}
		if degenerate {
			reasons = append(reasons, fmt.Sprintf("degenerate output (median %d completion tokens)", s.medianCompletionTokens))
		}
		if ex := s.excludedTotal(); ex > 0 {
			reasons = append(reasons, fmt.Sprintf("%d further failure(s) not charged to the model (%s)", ex, s.excludedBreakdown()))
		}

		rec := "no fallback configured — set modelFallback on the affected role(s)"
		if fb := fallbacks[s.model]; fb != "" {
			rec = fmt.Sprintf("recommend switching to configured modelFallback %q", fb)
		}
		findings = append(findings, modelHealthFinding{
			model:   s.model,
			status:  status,
			message: fmt.Sprintf("%s: %s; %s", s.model, strings.Join(reasons, ", "), rec),
		})
		if hostFinding != nil {
			findings = append(findings, *hostFinding) // after the model's own row
		}
	}
	// Stable: by model; within a model, the model finding before the host one.
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].model < findings[j].model })
	return findings
}

// hostCheckFor names the doctor check that covers an excluded class.
func hostCheckFor(class string) string {
	switch class {
	case stepoutcome.ClassAgentMountUnusable, stepoutcome.ClassContainerStartFailed:
		return "agent_image_uid and image_freshness"
	case stepoutcome.ClassMissingPrerequisite:
		return "the upstream step that should have produced the input"
	case stepoutcome.ClassWorkspaceUnavailable:
		return "orphan_worktrees and the project's git hooks (process-spawn law S6-D1)"
	default:
		return "the container runtime (podman) and host resources"
	}
}

// queryModelHealthOutcomes aggregates recent step outcomes per model: the
// CHARGED samples and failures, and the failures NOT charged to the model, by
// class (model-health attribution design, 2026-09-24). Portable SQL (FILTER,
// COALESCE, IN) — unlike the token-median query beside it — so it is tested
// against SQLite as well as run on Postgres.
func queryModelHealthOutcomes(ctx context.Context, db *sql.DB, since time.Time) (map[string]*modelHealthStat, error) {
	// 'ok' / 'pending_validation' are not failures, and the audit labels
	// 'superseded' / 'orphaned' are absences, not outcomes (2026-09-04). The
	// classes that are not the model's (model-health attribution design,
	// 2026-09-24) leave both counts and are reported per class instead.
	// The list is the Go declaration passed as parameters, so the SQL cannot
	// drift from stepoutcome.NotAttributableToModel.
	args := []any{since, modelHealthRowCap}
	var ph []string
	for _, c := range stepoutcome.NotAttributableToModelClasses() {
		args = append(args, c)
		ph = append(ph, fmt.Sprintf("$%d", len(args)))
	}
	excluded := "COALESCE(error_class, '') IN (" + strings.Join(ph, ", ") + ")"
	outcomeRows, err := db.QueryContext(ctx, `
		SELECT model,
		       COALESCE(error_class, '') AS error_class,
		       COUNT(*) FILTER (WHERE NOT excluded) AS samples,
		       COUNT(*) FILTER (WHERE NOT excluded AND outcome NOT IN ('ok', 'pending_validation', 'superseded', 'orphaned')) AS failures,
		       COUNT(*) FILTER (WHERE excluded AND outcome NOT IN ('ok', 'pending_validation')) AS excluded_failures
		FROM (
		    SELECT model, outcome, error_class, `+excluded+` AS excluded
		    FROM execution_step_outcomes
		    WHERE recorded_at >= $1 AND model <> ''
		      AND outcome NOT IN ('superseded', 'orphaned')
		    ORDER BY recorded_at DESC
		    LIMIT $2
		) recent
		GROUP BY model, COALESCE(error_class, '')
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query step outcomes: %w", err)
	}
	defer func() { _ = outcomeRows.Close() }()

	statByModel := map[string]*modelHealthStat{}
	for outcomeRows.Next() {
		var model, class string
		var samples, failures, excludedFailures int
		if err := outcomeRows.Scan(&model, &class, &samples, &failures, &excludedFailures); err != nil {
			// Fail, don't drop: a missing (model, class) row would understate
			// exactly the excluded counts this aggregation exists to report.
			return nil, fmt.Errorf("scan step outcomes: %w", err)
		}
		st, ok := statByModel[model]
		if !ok {
			st = &modelHealthStat{model: model}
			statByModel[model] = st
		}
		st.samples += samples
		st.failures += failures
		if excludedFailures > 0 {
			if st.excludedByClass == nil {
				st.excludedByClass = map[string]int{}
			}
			st.excludedByClass[class] += excludedFailures
		}
	}
	if err := outcomeRows.Err(); err != nil {
		return nil, fmt.Errorf("scan step outcomes: %w", err)
	}

	return statByModel, nil
}

// queryModelHealthStats is the default DB-backed source: per-model recent
// failure counts (execution_step_outcomes) joined with median completion
// tokens (task_llm_usage), bounded by window + row cap. Postgres-only SQL
// (production); tests inject a fake so SQLite need not parse it.
func (h *DoctorHandlers) queryModelHealthStats(ctx context.Context) ([]modelHealthStat, error) {
	since := time.Now().Add(-modelHealthWindow)

	// Failure aggregation from the purpose-built outcomes table. 'ok' and
	// 'pending_validation' (not yet finalized) are NOT failures; everything
	// else in the outcome taxonomy is. Row-capped via a bounded subquery.
	//
	// EXCEPT the two audit labels, which are absences rather than outcomes:
	// 'superseded' (an operator replaced the run) and 'orphaned' (the
	// execution went terminal by a path that never finalised the row). Neither
	// says anything about the MODEL, and counting them as failures charged a
	// model for attempts it may never have been asked to make — 809 such rows
	// existed on 2026-09-04, and they are excluded from every other quality
	// surface (quality_repository's canonStepFilter, workflow-stats' first-pass
	// denominator). This check was the one place still counting them.
	// AND the classes that are not the model's — see queryModelHealthOutcomes.
	statByModel, err := queryModelHealthOutcomes(ctx, h.db, since)
	if err != nil {
		return nil, err
	}

	// Median completion tokens per model from the usage table.
	tokenRows, err := h.db.QueryContext(ctx, `
		SELECT model,
		       PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY completion_tokens) AS median_completion
		FROM (
		    SELECT model, completion_tokens
		    FROM task_llm_usage
		    WHERE recorded_at >= $1 AND model <> ''
		    ORDER BY recorded_at DESC
		    LIMIT $2
		) recent
		GROUP BY model
	`, since, modelHealthRowCap)
	if err != nil {
		return nil, fmt.Errorf("query usage tokens: %w", err)
	}
	defer func() { _ = tokenRows.Close() }()

	for tokenRows.Next() {
		var model string
		var median sql.NullFloat64
		if err := tokenRows.Scan(&model, &median); err != nil {
			continue
		}
		st, ok := statByModel[model]
		if !ok {
			// A model with usage rows but no outcome rows: still track it so a
			// degenerate-token model that never reached the outcome finalizer
			// is visible. samples stays 0 → it's skipped by the min-sample
			// guard unless outcomes exist, which is the conservative default.
			st = &modelHealthStat{model: model}
			statByModel[model] = st
		}
		if median.Valid {
			st.medianCompletionTokens = int64(median.Float64)
		}
	}
	if err := tokenRows.Err(); err != nil {
		return nil, fmt.Errorf("scan usage tokens: %w", err)
	}

	out := make([]modelHealthStat, 0, len(statByModel))
	for _, st := range statByModel {
		out = append(out, *st)
	}
	return out, nil
}
