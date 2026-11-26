package api

// Doctor check: model_calls_live.
//
// INCIDENT 2026-07-30, customer deployment. `vornikctl doctor` reported "all 11
// role-pinned model(s) healthy" while six memory-pipeline models failed 100% of their
// calls for hours — roughly 500 `context deadline exceeded` per hour — and memory
// ingestion was completely stalled. The operator ran the one check they trust and it told
// them nothing was wrong.
//
// model_health could not have caught it, and that is structural rather than a bug:
//
//   - it enumerates models pinned by swarm ROLES, and the memory workers (classifier,
//     titler, reranker, graph extractor/resolver/validator) are daemon-level config;
//   - its sources cannot see the failures anyway — execution_step_outcomes needs an
//     execution step and there isn't one, while task_llm_usage is a SPEND table with no
//     error column, so a call that times out writes nothing at all.
//
// This check has no enumeration blind spot BY CONSTRUCTION: it reads outcomes recorded at
// the provider wrapper every model call already passes through (chat.CallStats), so a
// call site cannot be missed because nobody remembered to list it.
//
// HORIZON, stated in the finding itself: process-lifetime, reset on restart. model_health
// deliberately avoids that in favour of a DB window that survives a bounce, which is the
// right choice for "has this model been bad over 24h" and the wrong one for "is this
// model failing right now" — the question an operator actually asks when they run doctor
// after noticing trouble. The two are complementary, and the message says which it is so
// nobody mistakes it for history.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

const (
	// The sample floor and the failure rate are `doctor.thresholds` keys as of
	// 2026-09-13, SHARED with model_health rather than duplicated: the two
	// checks read the same question (is this model failing?) off different
	// evidence, and two knobs for one judgement is an invitation for a
	// deployment to answer it two ways.
	// modelCallsLiveMaxReported bounds the rendered list so one broken gateway cannot
	// produce a wall of text. Snapshot is sorted worst-first, so the cap keeps the
	// entries that matter.
	modelCallsLiveMaxReported = 6
)

// checkModelCallsLive flags (model, call_site) pairs whose calls are failing now.
//
// Read-only, no --fix: the remedy is always an operator decision — repoint a model, raise
// a timeout, or relieve whatever is saturating the endpoint — and none of those is safe to
// automate under a live deployment.
func (h *DoctorHandlers) checkModelCallsLive() DoctorCheck {
	name := "model_calls_live"

	snap := h.callStats.Snapshot()
	if len(snap) == 0 && h.callStats.DroppedGone() == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "no model calls observed since daemon start; nothing to assess",
		}
	}

	th := h.doctorThresholds()
	var (
		flagged  []string
		assessed int
		total    int
		failures int
	)
	gone := h.goneModelFindings(snap)
	for _, s := range snap {
		total += s.Calls
		failures += s.Failures
		if !s.GoneAt.IsZero() {
			continue // reported in its own list (#882)
		}
		if s.Calls < th.ModelMinSamples.Value {
			continue
		}
		assessed++
		if s.FailureRate() < th.ModelFailureRate.Value {
			continue
		}
		entry := fmt.Sprintf("%s via %s: %d/%d calls failing (%.0f%%)",
			s.Model, s.CallSite, s.Failures, s.Calls, s.FailureRate()*100)
		if s.LastError != "" {
			entry += " — last error: " + truncateDoctorError(s.LastError)
		}
		flagged = append(flagged, entry)
	}

	if len(flagged) == 0 && gone.lines == nil {
		return DoctorCheck{
			Name:   name,
			Status: "OK",
			Message: fmt.Sprintf(
				"%d model/call-site pair(s) assessed since daemon start, %d call(s) total, %d failed — none above the %.0f%% failure threshold (%s)",
				assessed, total, failures, th.ModelFailureRate.Value*100, th.ModelFailureRate.Source()),
		}
	}

	sort.Strings(flagged)
	if len(flagged) > modelCallsLiveMaxReported {
		dropped := len(flagged) - modelCallsLiveMaxReported
		flagged = flagged[:modelCallsLiveMaxReported]
		flagged = append(flagged, fmt.Sprintf("…and %d more pair(s) above the threshold", dropped))
	}
	if gone.lines != nil {
		return gone.check(name, flagged)
	}

	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: "model calls are failing since daemon start — every call site is covered here, " +
			"including the memory workers that model_health does not enumerate:\n  " +
			strings.Join(flagged, "\n  ") +
			"\n  A worker failing at this rate stalls the pipeline it feeds (memory " +
			"classification, titles, graph extraction) without failing any task, so it is " +
			"invisible everywhere else. Check whether the endpoint is saturated before " +
			"repointing the model: calls that are fast in isolation but time out under the " +
			"daemon's own concurrency mean congestion, not a bad model.",
	}
}

// truncateDoctorError bounds an upstream error string so one verbose provider cannot
// dominate the report.
func truncateDoctorError(s string) string {
	const limit = 120
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// modelHealthHealthySummary renders model_health's all-clear.
//
// It used to read "all N role-pinned model(s) healthy". Literally true — it had assessed
// exactly the role-pinned models — but on 2026-07-30 an operator read it as "the models
// are fine" while six models it never looked at were failing every call. A check must not
// imply coverage it does not have, so the summary now names its own scope and points at
// the check that covers the rest.
func modelHealthHealthySummary(count int) string {
	return fmt.Sprintf(
		"all %d role-pinned model(s) healthy over the recent window; models used outside swarm roles "+
			"(memory classifier/titler/reranker/graph) are NOT covered here — see model_calls_live",
		count)
}

// modelCallsLiveMaxGone bounds the retired-model list, separately from the
// rate list (GitHub #882, retired-model design §2 R2).
const modelCallsLiveMaxGone = 10

type goneFindings struct {
	lines []string
	inUse bool // a loaded role or tier still resolves to a gone model
}

// goneModelFindings lists every (model, call site) that answered HTTP 410
// since daemon start, naming the loaded roles and tiers that resolve to the
// model.
func (h *DoctorHandlers) goneModelFindings(snap []chat.CallStat) goneFindings {
	var g goneFindings
	users, loadErr := h.modelUsers()
	for _, s := range snap {
		if s.GoneAt.IsZero() {
			continue
		}
		line := fmt.Sprintf("%s answered 410 (retired) via %s at %s: %s", s.Model, s.CallSite,
			s.GoneAt.UTC().Format(time.RFC3339), truncateDoctorError(s.GoneMessage))
		switch u := users[s.Model]; {
		case len(u) > 0:
			line += "; used by " + strings.Join(u, ", ")
			g.inUse = true
		case loadErr != nil:
			// The roles were not examined, so "no role uses it" would be a guess
			// (review 20261122-237a F2): stay an ERROR and say why.
			line += "; roles not examined: the registry did not load (" + truncateDoctorError(loadErr.Error()) + ")"
			g.inUse = true
		default:
			line += "; no loaded role uses it now"
		}
		g.lines = append(g.lines, line)
	}
	sort.Strings(g.lines)
	if len(g.lines) > modelCallsLiveMaxGone {
		more := len(g.lines) - modelCallsLiveMaxGone
		g.lines = append(g.lines[:modelCallsLiveMaxGone], fmt.Sprintf("…and %d more retired model answer(s)", more))
	}
	if d := h.callStats.DroppedGone(); d > 0 {
		g.lines = append(g.lines, fmt.Sprintf("%d more 410 answer(s) not recorded (cap)", d))
	}
	return g
}

// modelUsers maps each model to the loaded roles ("<swarm>/<role>") and the
// tiers ("tier <name>") that resolve to it. err is a registry load failure
// other than a validation error (which still loads the valid swarms): the
// roles were then not examined.
func (h *DoctorHandlers) modelUsers() (map[string][]string, error) {
	out := map[string][]string{}
	for tier, c := range h.agentTiers {
		for _, m := range []string{c.Model, c.Fallback} {
			if m != "" {
				out[m] = append(out[m], "tier "+tier)
			}
		}
	}
	var loadErr error
	if h.configDir != "" {
		reg := registry.New()
		if err := reg.Load(h.configDir); err != nil {
			var ve *registry.ValidationError
			if !errors.As(err, &ve) {
				loadErr = err
			}
		}
		for _, sw := range reg.ListSwarms() {
			for _, r := range sw.Roles {
				for _, m := range []string{r.Model, r.ModelFallback} {
					if m != "" {
						out[m] = append(out[m], sw.ID+"/"+r.Name)
					}
				}
			}
		}
	}
	for m := range out {
		sort.Strings(out[m])
	}
	return out, loadErr
}

func (g goneFindings) check(name string, flagged []string) DoctorCheck {
	status := "WARNING"
	if g.inUse {
		status = "ERROR"
	}
	msg := "a provider answered HTTP 410 since daemon start: the model is retired. Repoint every role and tier named here:\n  " +
		strings.Join(g.lines, "\n  ")
	if len(flagged) > 0 {
		msg += "\n  Also failing above the rate threshold:\n  " + strings.Join(flagged, "\n  ")
	}
	return DoctorCheck{Name: name, Status: status, Message: msg}
}
