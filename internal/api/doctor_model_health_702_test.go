package api

import (
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/playbook"
)

// GitHub #702 (found in production 2026-11-09): doctor model_health put
// "(UPSTREAM_RATE_LIMITED)" in front of the operator, and the obvious next
// step — `vornikctl playbook show UPSTREAM_RATE_LIMITED` — answered
// "Unrecognised failure class". The remediation sits inside the LLM_CALL_FAILED
// entry; the lookup just did not know the code was covered there.
//
// Acceptance from the issue: every CODE doctor prints has a `playbook show`
// that does not hit the fallback. model_health is the only doctor check that
// prints an error_detail code, so this pins its output against the corpus.

var parenthesisedCodeRe = regexp.MustCompile(`\(([A-Z][A-Z0-9_]{3,})\)`)

// TestDoctorModelHealth_PrintedCodesResolveInThePlaybook — each finding's
// message may name codes in parentheses; every one of them must resolve to a
// real playbook entry, never the unknown-class fallback. Fails pre-fix:
// UPSTREAM_RATE_LIMITED resolved to "Unrecognised failure class".
func TestDoctorModelHealth_PrintedCodesResolveInThePlaybook(t *testing.T) {
	stats := []modelHealthStat{{
		model: "glm-5.3-flash", samples: 40, failures: 40, rateLimited: 38,
	}}
	findings := evalModelHealth(stats, oneFallback("glm-5.3-flash", "zai.glm-5"), defaultDoctorThresholds())
	if len(findings) != 1 {
		t.Fatalf("want one finding, got %v", findings)
	}
	codes := parenthesisedCodeRe.FindAllStringSubmatch(findings[0].message, -1)
	if len(codes) == 0 {
		t.Fatal("the rate-limited reason names no code in parentheses — the test would pass vacuously")
	}
	for _, m := range codes {
		code := m[1]
		entry := playbook.Lookup(code)
		if strings.Contains(entry.Cause, "Unrecognised") {
			t.Errorf("doctor prints %q but `playbook show %s` hits the unknown-class fallback", code, code)
			continue
		}
		// The fallback Entry sets Class==code and leaves ResolvedFrom empty, so
		// only ResolvedFrom distinguishes a covered code from an unrecognised one
		// whose class happens to match. Pin it: the entry must record that it was
		// resolved FROM this code (the alias path), not merely carry its name.
		if entry.ResolvedFrom != code {
			t.Errorf("doctor prints %q; `playbook show %s` did not record it as resolved from that code (resolvedFrom=%q, class=%q)",
				code, code, entry.ResolvedFrom, entry.Class)
		}
	}
}
