package memory

import (
	"strings"
	"testing"
)

// #618 C1/C2 (firewall design, "Amendment 2026-11-06 (#618)"): the
// sensitivity-cap fragment. A role capped at public sees only chunks set
// public by an operator or derived public (repo-scoped, provenance-verified,
// under a configured public prefix); an unlabelled chunk ranks as
// confidential; credentials are excluded under any cap; an unknown cap or an
// invalid public prefix admits nothing.
func TestScopeFilterSQL_MaxSensitivity_618(t *testing.T) {
	derived := "(sensitivity_tier IS NULL AND repo_scope IS NOT NULL AND validation_status = 'verified' AND validator_role IN ('" +
		ValidatorRoleProvenance + "', '" + ValidatorRoleProvenanceForgeAPI + "') AND (source_name LIKE 'docs/public/%' ESCAPE '\\'))"
	noCred := "AND content_class IS DISTINCT FROM 'credentials'"
	prefixes := []string{"docs/public/"}

	cases := []struct {
		cap  string
		want []string
	}{
		{SensitivityCapPublic, []string{noCred, "AND (sensitivity_tier = 'public' OR " + derived + ")"}},
		{SensitivityCapInternal, []string{noCred, "AND (sensitivity_tier IN ('public', 'internal') OR " + derived + ")"}},
		{SensitivityCapConfidential, []string{noCred, "AND (sensitivity_tier IS NULL OR sensitivity_tier <> 'restricted')"}},
	}
	for _, c := range cases {
		got := ScopeFilter{MaxSensitivity: c.cap, PublicPrefixes: prefixes}.sql(3)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("cap %s: missing %s\n got: %s", c.cap, w, got)
			}
		}
	}

	// No public prefixes: nothing is derived public, only an operator label.
	got := ScopeFilter{MaxSensitivity: SensitivityCapPublic}.sql(3)
	if !strings.Contains(got, "AND (sensitivity_tier = 'public' OR FALSE)") {
		t.Errorf("without prefixes nothing may be derived public; got: %s", got)
	}

	// Fail closed: an unknown cap, or one invalid public prefix.
	for _, bad := range []string{"secret", "restricted", "PUBLIC"} {
		if got := (ScopeFilter{MaxSensitivity: bad, PublicPrefixes: prefixes}).sql(3); !strings.HasSuffix(got, " AND FALSE") {
			t.Errorf("cap %q must admit nothing; got: %s", bad, got)
		}
	}
	for _, bad := range []string{"docs/%/", "docs/x'--/", "docs/public", ""} {
		got := ScopeFilter{MaxSensitivity: SensitivityCapPublic, PublicPrefixes: []string{"docs/public/", bad}}.sql(3)
		if !strings.Contains(got, "AND (sensitivity_tier = 'public' OR FALSE)") {
			t.Errorf("public prefix %q must derive nothing; got: %s", bad, got)
		}
	}

	if (ScopeFilter{MaxSensitivity: SensitivityCapPublic}).IsZero() {
		t.Error("a cap-only filter must not count as zero")
	}
	// Prefixes alone, with no cap, add nothing: they serve only the cap.
	if got, want := (ScopeFilter{Strict: true, PublicPrefixes: prefixes}).sql(3), scopeFilterSQL(true, 3); got != want {
		t.Errorf("public prefixes without a cap must add nothing:\n got %s\nwant %s", got, want)
	}
}
