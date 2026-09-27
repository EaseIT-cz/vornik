package api

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/configdrift"
	"vornik.io/vornik/internal/version"
)

// checkConfigTemplateDrift reports deployed configs that differ from what this
// version ships (config-tree drift design, third amendment; issue #61(a)).
//
// The deployed tree is preserve-existing — correct, because Vornik tunes it —
// so a template FIX never arrives and nothing said so: a CE operator's
// dev-pipeline.md predated the `recovery: true` marker the firing
// workflow_onfail_masking check honours, and read like a product bug.
//
// It compares the deployed tree against the baselines the installer keeps
// (package configdrift), and follows the design's precedence table:
//
//	no config dir / no baseline      SKIPPED (not evaluated)
//	baseline unstamped               WARNING; tunable findings suppressed,
//	                                 canonical divergence named
//	baseline stale (not this binary) WARNING — the finding is the deploy route;
//	                                 tunable suppressed, canonical named
//	current baseline                 WARNING per finding, else OK with both
//	                                 denominators
//
// A stale or unstamped reference would turn tuning into false "predates the
// template" findings — the CE false positive this check exists to explain — so
// tunable findings wait for a trustworthy baseline. A canonical asset has no
// tuning to misread, so its divergence is never suppressed.
//
// WARNING, never ERROR: a stale config runs; it lacks a later fix. A declined
// change is acknowledged with `vornikctl doctor ack config_template_drift
// <file>` (POST /api/v1/doctor/ack); acknowledged findings are suppressed and
// counted, keyed so a genuinely new finding re-opens.
func (h *DoctorHandlers) checkConfigTemplateDrift() DoctorCheck {
	const name = "config_template_drift"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config directory configured, skipping"}
	}
	r, err := configdrift.Compare(h.configDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not compare the deployed configs against their templates: " + err.Error()}
	}
	if r.Baseline == configdrift.BaselineAbsent {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: fmt.Sprintf(
			"no template baseline under %s/.templates — the manifest-driven installer (make install-config-assets, "+
				"scripts/config-deploy.sh) records one; packaged and quickstart installs do not run it yet, so this "+
				"check cannot say whether the deployed configs predate their templates", h.configDir)}
	}
	acks, aerr := configdrift.ReadAcks(h.configDir)
	rd := &driftRender{acks: acks, ledger: &driftLedger{l: h.workflowProposals}}
	if aerr != nil {
		// Unreadable acks fail OPEN to un-acked: every finding renders, and
		// the row says why nothing was suppressed.
		rd.acks = nil
		rd.items = append(rd.items, "acknowledgements could not be read ("+aerr.Error()+"); every finding is shown unacknowledged")
	}

	current, why := h.baselineCurrency(r)
	for _, ff := range r.Files {
		if ff.Class == configdrift.Canonical && ff.CanonicalDiverged {
			k, _ := ff.CanonicalKey()
			rd.add(&rd.hard, ff.Rel, k, canonicalItem(ff.Rel, r.Baseline, current))
		}
	}
	for _, rm := range r.Removed {
		switch rm.Class {
		case configdrift.Canonical:
			rd.add(&rd.hard, rm.Rel, rm.Key(), fmt.Sprintf("%s: the template stopped shipping this canonical file in %s; the deployed copy is still loaded", rm.Rel, rm.Revision))
		case configdrift.Unknown:
			// A deletion is independent of the tunable axis, so it still
			// renders when the axis is missing — named as unclassed (round 8 N4).
			rd.add(&rd.hard, rm.Rel, rm.Key(), fmt.Sprintf("%s: the template stopped shipping this file in %s; the deployed copy is still loaded (class unknown: the baseline records no class axis)", rm.Rel, rm.Revision))
		}
	}
	canonicalItems := len(rd.items)
	if current {
		for _, ff := range r.Files {
			if ff.Class == configdrift.Tunable {
				rd.tunable(ff)
			}
		}
		for _, rel := range r.MissingFiles {
			rd.items = append(rd.items, rel+": shipped by this version and not deployed at all (preserve-existing never creates a file the tree already lacked)")
			rd.hard++
		}
		for _, rm := range r.Removed {
			if rm.Class == configdrift.Tunable {
				rd.add(&rd.hard, rm.Rel, rm.Key(), fmt.Sprintf("%s: the template stopped shipping this file in %s; the deployed copy is still loaded", rm.Rel, rm.Revision))
			}
		}
	}

	denom := fmt.Sprintf("compared %d deployed file(s) against %d template(s) (baseline %s)",
		r.DeployedCompared, r.TemplateFiles, shortRevision(r.Stamp))
	if rd.acked > 0 {
		denom += fmt.Sprintf("; %d finding(s) acknowledged", rd.acked)
	}
	denom += rd.ledger.denominator()
	if !current {
		msg := fmt.Sprintf("the template baseline is %s — %s; per-file findings for tunable configs are suppressed until it is re-recorded, because a stale reference would read tuning as missed fixes",
			why, "re-run the manifest-driven installer (make install-config-assets) so the baseline matches this binary")
		if canonicalItems > 0 {
			msg += fmt.Sprintf("; %d canonical finding(s) below are reported regardless", canonicalItems)
		}
		return DoctorCheck{Name: name, Status: "WARNING", Message: msg + ". " + denom, Items: rd.items}
	}
	if len(rd.items) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "every deployed config carries its template's content, or its difference is acknowledged; " + denom}
	}
	// The two classes are counted separately, so a large soft count cannot
	// swamp a small hard one, and a clean hard half cannot read as the whole.
	return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
		"%d finding(s) where a deployed config lacks what this version ships, and %d file(s) carrying lines the template does not "+
			"(tuning or template removals, not distinguishable without an .origin record); %s. "+
			"Deployed configs are preserved on upgrade on purpose (Vornik tunes them), so a template fix is not applied "+
			"for you: take the change by editing the deployed file, or record that you decline it with "+
			"`vornikctl doctor ack config_template_drift <file>`. This row recurs until one of the two.",
		rd.hard, rd.soft, denom), Items: rd.items}
}

// driftRender applies the acknowledgement precedence (drift design, slice C)
// while building the row's items.
type driftRender struct {
	acks              *configdrift.Acks
	ledger            *driftLedger
	items             []string
	hard, soft, acked int
}

// status is the finding's standing against the acks, and the sentence (if
// any) it adds.
func (rd *driftRender) status(rel string, k configdrift.AckKey) (suppress bool, suffix string) {
	st, rec := rd.acks.StatusRecord(rel, k)
	// "by <actor>" when the record names one (drift design slice F); a line
	// from before slice F keeps today's wording.
	by := ""
	if rec.Actor != "" {
		by = " by " + rec.Actor
	}
	switch st {
	case configdrift.Acked:
		return true, ""
	case configdrift.NowClassifiable:
		return false, " — now classifiable: the acknowledgement" + by + " of " + rec.Date + " covered the unclassified form"
	case configdrift.AckMissing:
		return false, " — acknowledged" + by + " on " + rec.Date + ", acknowledgement missing from the store (restored?); re-run `vornikctl doctor ack config_template_drift " + rel + "` to restore it"
	}
	return false, ""
}

// add renders one single-key finding unless it is acknowledged.
func (rd *driftRender) add(counter *int, rel string, k configdrift.AckKey, item string) {
	suppress, suffix := rd.status(rel, k)
	if suppress {
		rd.acked++
		return
	}
	rd.items = append(rd.items, item+suffix)
	*counter++
}

// tunable renders one tunable file: its unacknowledged hard hunks (one item
// for the file, naming only the hunks still open), its exact-mode removals,
// and its vague soft class — each bounded.
func (rd *driftRender) tunable(ff configdrift.FileFinding) {
	var open []string
	var suffix string
	ev := rd.ledger.eval(ff)
	for _, h := range ff.HardHunks() {
		// An approved change explains it (slice E): not a finding, counted.
		if ev.exempt[h.Index] {
			rd.ledger.explained++
			continue
		}
		suppress, sfx := rd.status(ff.Rel, h.Key)
		if suppress {
			rd.acked++
			continue
		}
		open = append(open, h.Lines...)
		if sfx != "" {
			suffix = sfx
		}
	}
	if len(open) > 0 {
		for _, u := range ev.unrecorded {
			suffix += "; " + u + " may explain this hunk; its pre-apply file was not recorded"
		}
		rd.items = append(rd.items, fmt.Sprintf("%s: the template has %d line(s) the deployed file lacks: %s%s",
			ff.Rel, len(open), quoteLines(firstN(open, configdrift.MaxHunkLines)), suffix))
		rd.hard++
	}
	k, ok := ff.SoftKey()
	if !ok {
		return
	}
	if ff.Exact {
		rd.add(&rd.hard, ff.Rel, k, fmt.Sprintf("%s: %d line(s) the template removed since %s are still present: %s",
			ff.Rel, len(ff.RemovedByTemplate), shortRevision(ff.OriginRevision), quoteLines(ff.RemovedByTemplate)))
		return
	}
	// The vague soft class: a COUNT and the largest hunk's opening lines,
	// never the listing — enumerating it is the 1,466-line noise the design
	// bounds against. It exists to make someone diff, not to be the diff.
	rd.add(&rd.soft, ff.Rel, k, fmt.Sprintf("%s: carries %d line(s) the template does not — tuning, or lines a later template removed; "+
		"indistinguishable here (%s). Largest such block begins: %s",
		ff.Rel, ff.SoftCount, ff.VagueReason, quoteLines(firstN(ff.SoftLargest, 3))))
}

// baselineCurrency reports whether the baseline can be trusted for tunable
// findings, and if not, why.
func (h *DoctorHandlers) baselineCurrency(r *configdrift.Report) (bool, string) {
	if r.Baseline == configdrift.BaselineUnstamped {
		return false, "unstamped (it records no revision or no class axis, so it cannot vouch for itself)"
	}
	br := h.buildRevision
	if br == nil {
		br = version.BuildRevision
	}
	rev, dirty, ok := br()
	if !ok {
		// Nothing to compare the stamp with. The baseline is stamped, so trust
		// it; the message's baseline revision lets the operator judge.
		return true, ""
	}
	if !stampMatches(r.Stamp, rev, dirty) {
		shown := rev
		if dirty {
			shown += "-dirty"
		}
		return false, fmt.Sprintf("stale (recorded by %s; this daemon is %s)", shortRevision(r.Stamp), shown)
	}
	return true, ""
}

// stampMatches compares the installer's stamp — the FULL sha from `git
// rev-parse HEAD`, with "-dirty" appended for a dirty tree (Makefile
// VORNIK_REVISION) — with the running binary's revision, which
// version.BuildRevision SHORTENS to 12 characters and reports dirtiness
// separately. The first deploy compared them with ==, which can never hold on
// a real build, and read a current baseline as stale (2026-09-24, on this
// host: "recorded by de573c9bedb3; this daemon is de573c9bedb3"). A prefix of
// at least 7 characters (git's own short form) is required, so an empty or
// truncated revision never matches.
func stampMatches(stamp, rev string, dirty bool) bool {
	stampDirty := strings.HasSuffix(stamp, "-dirty")
	sha := strings.TrimSuffix(stamp, "-dirty")
	if len(rev) < 7 || stampDirty != dirty {
		return false
	}
	return strings.HasPrefix(sha, rev) || strings.HasPrefix(rev, sha)
}

func canonicalItem(rel string, state configdrift.BaselineState, current bool) string {
	switch {
	case state == configdrift.BaselineUnstamped:
		return rel + ": diverged from its template; baseline unstamped, cannot classify — re-run the installer"
	case !current:
		return rel + ": diverged from its template; baseline also stale — re-deploy, then re-measure"
	}
	return rel + ": this canonical file differs from what this version ships (a canonical file is not tuned in place; if the change is a deliberate override, keep it)"
}

func quoteLines(lines []string) string {
	q := make([]string, 0, len(lines))
	for _, l := range lines {
		q = append(q, "+ "+strings.TrimSpace(l))
	}
	return strings.Join(q, "; ")
}

func shortRevision(rev string) string {
	if rev == "" {
		return "unstamped"
	}
	if len(rev) > 12 && !strings.Contains(rev[:12], "-") {
		return rev[:12]
	}
	return rev
}

func firstN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	return lines
}
