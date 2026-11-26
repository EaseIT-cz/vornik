package architecture

// GitHub #888: the pre-filter must never hide a match. These pin the literal
// derivation on the shapes the law regexes use, and check every whole-tree
// law regex against a corpus of lines it must match: the pre-filter has to
// pass each one. (A whole-tree with/without comparison would rerun the slow
// regexes this change removes; the literals are derived from the parsed
// regex, so they cannot go stale.)

import (
	"regexp"
	"strings"
	"testing"
)

func TestRequiredLiterals_888(t *testing.T) {
	for _, c := range []struct {
		re   string
		want []string // the exact derived set; nil: no pre-filter
	}{
		{`\.IsAdminKey\(|IsAdminClassRequest\(`, []string{".IsAdminKey(", "IsAdminClassRequest("}},
		{`persistence\.APIKey\{|[^.\w]APIKey\{`, []string{"persistence.APIKey{", "APIKey{"}},
		{`(?:=|:)\s*(?:c\.)?repos\.LLMUsage\b|WithLLMUsageRepository\(`, []string{"repos.LLMUsage", "WithLLMUsageRepository("}},
		{`actor\.Autonomy\b`, []string{"actor.Autonomy"}},
		{`(?i)secret`, nil},   // case-insensitive: bytes.Contains cannot filter
		{`[A-Z]+|token`, nil}, // a branch with no literal
		{`(?:abc)?def`, []string{"def"}},
		{`(?:abc)*`, nil}, // can match the empty string
		{`\w+`, nil},
	} {
		var got []string
		for _, l := range requiredLiterals(regexp.MustCompile(c.re)) {
			got = append(got, string(l))
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%s: literals %q, want %q", c.re, got, c.want)
		}
	}
}

// Each whole-tree law regex, with lines it must match. The pre-filter must
// let every one through, and the regex must match it (so the corpus itself
// stays honest).
func TestRegexPrefilterNeverHidesAMatch_888(t *testing.T) {
	corpus := map[*regexp.Regexp][]string{
		adminClassResolverRe: {`if s.IsAdminKey(k) {`, `return IsAdminClassRequest(r)`, `isAdminClassKey(key)`, `api.IsAdminFromContext(ctx)`},
		credentialReaderRe:   {`r.Header.Get("Authorization")`, `r.Header.Values("X-API-Key")`, `u, p, ok := r.BasicAuth()`, `r.URL.Query().Get("token")`},
		wiringSitePattern:    {`LLMUsage: c.repos.LLMUsage,`, `x := repos.LLMUsage`, `WithLLMUsageRepository(repo)`},
		apiKeyMintPattern:    {`k := &persistence.APIKey{`, `return APIKey{ID: id}`},
	}
	for _, law := range provenancePrincipalLaws {
		corpus[law.re] = append(corpus[law.re], law.example)
	}
	// The other branch of each alternation, so both sides are exercised.
	for _, law := range provenancePrincipalLaws {
		switch law.name {
		case "a COMPANION task is created":
			corpus[law.re] = append(corpus[law.re], "CreationSource: persistence.TaskCreationSourceCompanion,")
		case "a chat task's principal is set":
			corpus[law.re] = append(corpus[law.re], "CreatorActor: actor,")
		}
	}
	for re, lines := range corpus {
		for _, line := range lines {
			if !re.MatchString(line) {
				t.Errorf("corpus line %q does not match %s: fix the corpus", line, re)
				continue
			}
			if !regexMayMatch(re, []byte(line)) {
				t.Errorf("the pre-filter hides a match of %s in %q", re, line)
			}
		}
	}
}
