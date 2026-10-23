package architecture

// GitHub #482 round-3 review A2, A3, A4 and #499 (design 22, "Amendment
// 2026-10-21 (#482)", parts 8a to 8c). Memory verification trusts a task
// chain's root only with an attributable operator principal, and trusts that
// principal because a single server-side writer stamps each kind. These laws
// fail when a second writer appears:
//
//   - only internal/autonomy creates an AUTONOMOUS task or stamps
//     actor.Autonomy (the pair the predicate trusts);
//   - only internal/api's companion surfaces create a COMPANION task;
//   - only internal/api/task_principal.go stamps the operator-key principal;
//   - every DB key mint site is a known one, and each tags its class (a
//     companion key's client_kind, the agent admin's flags, the executor's
//     agent: names) or is the operator's own plain key.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// nonTestGoFiles lists the module's non-test Go files under internal/ and
// cmd/, module-relative, excluding the shared test suites in repotest.
func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	root, _ := moduleRootAndPath(t)
	var out []string
	for _, top := range []string{"internal", "cmd"} {
		walkRoot := filepath.Join(root, top)
		err := filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(walkRoot, p, d.Name(), "repotest", "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				rel, _ := filepath.Rel(root, p)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	requireExamined(t, "provenance principal law", "non-test Go files", len(out), 500)
	return out
}

func filesMatching(t *testing.T, files []string, re *regexp.Regexp) []string {
	t.Helper()
	root, _ := moduleRootAndPath(t)
	var hit []string
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if re.Match(b) {
			hit = append(hit, f)
		}
	}
	sort.Strings(hit)
	return hit
}

func TestProvenancePrincipalWriters(t *testing.T) {
	files := nonTestGoFiles(t)
	for _, law := range []struct {
		name    string
		re      *regexp.Regexp
		allowed map[string]bool
	}{
		{"an AUTONOMOUS task is created", regexp.MustCompile(`CreationSource:\s*persistence\.TaskCreationSourceAutonomous`),
			map[string]bool{"internal/autonomy/manager.go": true}},
		{"the autonomy principal is stamped", regexp.MustCompile(`actor\.Autonomy\b`),
			map[string]bool{"internal/autonomy/manager.go": true}},
		{"a COMPANION task is created", regexp.MustCompile(`(?:CreationSource|source):\s*persistence\.TaskCreationSourceCompanion`),
			map[string]bool{"internal/api/companion_mcp.go": true, "internal/api/companion_broker.go": true}},
		{"the operator-key principal is stamped", regexp.MustCompile(`"operator_key:"`),
			map[string]bool{"internal/api/task_principal.go": true}},
		// Review 6262 F2: a chat task's principal is set from the request's
		// context in one place, never from a decoded field.
		{"a chat task's principal is set", regexp.MustCompile(`CreatorActor:\s|\.CreatorActor\s*=[^=]`),
			map[string]bool{"internal/dispatcher/tools.go": true}},
	} {
		for _, f := range filesMatching(t, files, law.re) {
			if !law.allowed[f] {
				t.Errorf("%s in %s: only %v may (design 22, part 8b)", law.name, f, keys(law.allowed))
			}
		}
	}
}

// keyMintSites are the non-test files that build a persistence.APIKey value,
// and what each mints. A new one fails until it is classified here: a plain
// key counts as the operator's for memory verification (design 22, part 8c).
var keyMintSites = map[string]string{
	"internal/api/admin_companion_handlers.go":    "companion key: client_kind required and pattern-checked",
	"internal/api/admin_companion_agent_grant.go": "agent admin key: agent_admin and agent_namespace set",
	"internal/api/api_key_handlers.go":            "the operator's project key (admin REST): plain, trusted",
	"internal/ui/project_keys.go":                 "the operator's project key (admin UI): plain, trusted",
	"internal/taskkeys/minter.go":                 "agent per-task and warm-pool keys: agent: names, never plain",
	"internal/api/broker_schedule_fire.go":        "an in-memory judge principal (agent_admin), never stored",
	"internal/service/agentadmin_agents.go":       "an in-memory reader principal (agent_admin), never stored",
	"internal/service/agentadmin_read.go":         "an in-memory grant principal (agent_admin), never stored",
	"internal/persistence/models.go":              "the type's own definition and helpers",
}

func TestKeyMintSitesTagTheirClass(t *testing.T) {
	files := nonTestGoFiles(t)
	re := regexp.MustCompile(`persistence\.APIKey\{|[^.\w]APIKey\{`)
	found := filesMatching(t, files, re)
	t.Logf("examined %d files; %d build an APIKey", len(files), len(found))
	seen := map[string]bool{}
	for _, f := range found {
		seen[f] = true
		if _, ok := keyMintSites[f]; !ok {
			t.Errorf("%s builds a persistence.APIKey and is not a known key mint site: classify it in keyMintSites (design 22, part 8c)", f)
		}
	}
	for f := range keyMintSites {
		if !seen[f] {
			t.Errorf("keyMintSites names %s, which no longer builds an APIKey: remove it", f)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOperatorKeyPredicateIsOneValue (review 7262; design 22, part 8l): REST
// stamps system:operator_key through api's adminClassKeys and the UI through
// its operatorCapability. Both must be built from the one admin config, so
// the two surfaces call the same keys the operator's.
func TestOperatorKeyPredicateIsOneValue(t *testing.T) {
	root, _ := moduleRootAndPath(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "service", "container_http.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api.WithAdminClassKeys(c.Config.Admin)", "ui.WithOperatorCapability(c.Config.Admin)"} {
		if n := strings.Count(string(src), want); n != 1 {
			t.Errorf("container_http.go has %d of %s, want exactly 1", n, want)
		}
	}
}
