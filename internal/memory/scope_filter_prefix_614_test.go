package memory

import (
	"strings"
	"testing"
)

// #614 C3: the source-prefix fragment. Each prefix matches literally (LIKE
// with its wildcards escaped), the prefixes are OR-ed inside one AND, a
// prefix the shared validator refuses makes the filter admit nothing, and a
// prefix-only filter is not zero (it would take the unscoped statement).
func TestScopeFilterSQL_SourcePrefixes_614(t *testing.T) {
	got := ScopeFilter{SourcePrefixes: []string{"docs/architect_x/", "docs/public/"}}.sql(3)
	for _, want := range []string{
		`AND (source_name LIKE 'docs/architect\_x/%' ESCAPE '\' OR source_name LIKE 'docs/public/%' ESCAPE '\')`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n got: %s", want, got)
		}
	}
	for _, bad := range []string{"docs/%/", "docs/x'--/", "docs/public", "/docs/public/", "docs/../legal/", ""} {
		if got := (ScopeFilter{SourcePrefixes: []string{"docs/public/", bad}}).sql(3); !strings.HasSuffix(got, " AND FALSE") {
			t.Errorf("prefix %q must fail closed; got: %s", bad, got)
		}
	}
	if (ScopeFilter{SourcePrefixes: []string{"docs/public/"}}).IsZero() {
		t.Error("a prefix-only filter must not count as zero")
	}
	if got, want := (ScopeFilter{Strict: true}).sql(3), scopeFilterSQL(true, 3); got != want {
		t.Errorf("no prefixes must add nothing:\n got %s\nwant %s", got, want)
	}
}
