package service

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): the writers of an approval's account and
// the callers that suspend standing grants are a closed set
// (agent-administered design §22.2.2, §22.2.14 items 4 and 6). A new call
// site fails here until the design names it.

// callSites lists, for each pattern, the non-test Go files under internal/
// (outside internal/persistence, whose drivers implement the methods) that
// call it, with the number of calls.
func callSites(t *testing.T, pattern string) map[string]int {
	t.Helper()
	re := regexp.MustCompile(pattern)
	out := map[string]int{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == filepath.Join("..", "persistence") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := len(re.FindAllIndex(b, -1)); n > 0 {
			out[filepath.ToSlash(strings.TrimPrefix(path, "../"))] = n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sitesString(m map[string]int) string {
	var s []string
	for f, n := range m {
		s = append(s, f+"="+strings.Repeat("|", n))
	}
	sort.Strings(s)
	return strings.Join(s, " ")
}

func TestAgentIntegrationApprovals_Writers(t *testing.T) {
	if got := sitesString(callSites(t, `\.UpsertIntegration\(`)); got != "service/agentadmin_recipes.go=| service/agentadmin_service.go=|" {
		t.Fatalf("UpsertIntegration callers: %s", got)
	}
	if got := sitesString(callSites(t, `\.SetIntegrationIdentity\(`)); got != "" {
		t.Fatalf("SetIntegrationIdentity callers outside transaction-bound persistence: %s", got)
	}
}

func TestSuspendGrantsThrough_Callers(t *testing.T) {
	got := sitesString(callSites(t, `\.suspendGrantsThrough\(`))
	const want = "service/agentadmin_account_transitions.go=|| service/agentadmin_service.go=|"
	if got != want {
		t.Fatalf("suspendGrantsThrough callers: %s, want %s", got, want)
	}
}

func TestStaticNamespaceSuspension_Callers(t *testing.T) {
	if got := sitesString(callSites(t, `\.suspendNamespaceGrants\(`)); got != "service/agentadmin_credentials.go=|" {
		t.Fatalf("static namespace suspension callers: %s", got)
	}
}

func TestAccountTransition_PublicationCallers(t *testing.T) {
	for pattern, want := range map[string]string{
		`\.AccountTransitions\.BeginConfig\(`: "service/agentadmin_account_transitions.go=|",
		`\.beginConfigAccountTransition\(`:    "service/agentadmin_service.go=||",
		`\.AccountTransitions\.Finish\(`:      "service/agentadmin_account_transitions.go=| service/agentadmin_credentials.go=| service/agentadmin_service.go=||",
		`\.PublishAccount\(`:                  "mcpconnect/connector.go=||",
		`\.PrepareSealedToken\(`:              "agenttokens/agenttokens.go=| service/agentadmin_account_transitions.go=|",
	} {
		if got := sitesString(callSites(t, pattern)); got != want {
			t.Fatalf("account publication %s callers: %s, want %s", pattern, got, want)
		}
	}
	b, err := os.ReadFile("container_mcp_oauth.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "PublishAccount: c.publishOAuthAccount") {
		t.Fatal("common device/admin connector account publisher is not installed")
	}
}
