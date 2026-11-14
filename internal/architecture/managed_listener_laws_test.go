package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Managed hosting's listener laws (GitHub #560; 18-security-and-tenancy.md,
// "Amendment 2026-10-28 (#560, slice M1b)", B5). Invariant 1: nothing
// admin-class is accepted on the public listener. M1a's listener gate refuses
// admin keys before routing and demotes admin sessions; these censuses keep
// that true as the code grows: a new place that decides admin-class, a new
// place that reads a credential, or a tenant_admin grant inside an admin-only
// family fails here until it is reviewed and listed. Files absent from the
// tree (the Community export) are skipped.

func censusCount(t *testing.T, re *regexp.Regexp, skip func(string) bool) map[string]int {
	t.Helper()
	root, _ := moduleRootAndPath(t)
	got := map[string]int{}
	for _, f := range nonTestGoFiles(t) {
		if skip != nil && skip(f) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if n := len(re.FindAll(b, -1)); n > 0 {
			got[f] = n
		}
	}
	return got
}

func compareCensus(t *testing.T, what string, got map[string]int, want map[string]int) {
	t.Helper()
	root, _ := moduleRootAndPath(t)
	var bad []string
	for f, n := range got {
		if want[f] != n {
			bad = append(bad, f)
		}
	}
	for f, n := range want {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			continue
		}
		if got[f] != n {
			bad = append(bad, f)
		}
	}
	sort.Strings(bad)
	seen := map[string]bool{}
	for _, f := range bad {
		if seen[f] {
			continue
		}
		seen[f] = true
		t.Errorf("%s: %s has %d site(s), the census lists %d: review it (18-security-and-tenancy.md, M1b B5) and list it here", what, f, got[f], want[f])
	}
	t.Logf("%s census: %d file(s) with a site", what, len(got))
}

// The resolver test: every place that decides admin-class. Each sits behind
// the listener gate on the public listener (the gate refuses admin keys
// before routing; admin sessions are demoted by the one session-role writer).
var adminClassResolvers = map[string]int{
	"internal/admin/middleware.go":     3,
	"internal/api/admin_class_auth.go": 3,
	"internal/api/api.go":              2, // isAdminClassKey: both admin-key sources on one line
	"internal/api/cpc_handlers.go":     1,
	// GitHub #582 P4: the rotation reads the presented key's admin class to
	// decide its slot; the listener gate refuses admin keys on the public
	// listener first, so it is reached on the operations listener only.
	"internal/api/credentials_rotate_582.go": 1,
	// GitHub #581 D5: registering an export takes an admin-class key (the
	// control plane's); the listener gate refuses admin keys on the public
	// listener first. The download beside it reads no admin class.
	"internal/api/owner_export_581.go":   1,
	"internal/api/middleware.go":         2,
	"internal/api/reminders_handlers.go": 2,
	"internal/api/routes.go":             2,
	"internal/ui/admin.go":               1,
	// GitHub #623: the credentials page's surface gate (OperatorSurfaceAccessFor
	// takes the admin-key checker) and its IsAdmin nav flag. On a managed
	// instance's public listener the listener gate refuses admin keys first,
	// so a tenant_admin session is admitted only through the granted surface.
	"internal/ui/admin_credentials.go": 2,
	// GitHub #754: the keys page's POST gate (OperatorSurfaceAccessFor with
	// the admin-key checker, surface project_keys). As for credentials, the
	// listener gate refuses admin keys on the public listener first.
	"internal/ui/project_keys.go": 1,
	// GitHub #759: the Manage overview shows an admin every card. A view,
	// not a gate: each card's destination keeps its own server gate.
	"internal/ui/manage.go":            1,
	"internal/ui/chat.go":              1,
	"internal/ui/fixit_doctor.go":      1,
	"internal/ui/memory.go":            1,
	"internal/ui/memory_operators.go":  3,
	"internal/ui/operator_accounts.go": 2,
	"internal/ui/reminders.go":         1,
	"internal/ui/server.go":            1,
}

var adminClassResolverRe = regexp.MustCompile(`\.IsAdminKey\(|IsAdminClassRequest\(|isAdminClassKey\(|IsAdminFromContext\(`)

func TestManagedListener560_ResolverCensus(t *testing.T) {
	// A function's own declaration line is not a call site.
	declRe := regexp.MustCompile(`(?m)^func [^\n]*(IsAdminKey|IsAdminClassRequest|isAdminClassKey|IsAdminFromContext)\(`)
	got := map[string]int{}
	root, _ := moduleRootAndPath(t)
	for f, n := range censusCount(t, adminClassResolverRe, nil) {
		b, _ := os.ReadFile(filepath.Join(root, f))
		if n -= len(declRe.FindAll(b, -1)); n > 0 {
			got[f] = n
		}
	}
	compareCensus(t, "admin-class resolver", got, adminClassResolvers)
}

// The key-reader test: every place that reads a credential header, Basic or
// a credential query value. A new form the listener gate does not read would
// let an admin key around it.
var credentialReaders = map[string]int{
	"internal/api/middleware.go":          3, // extractAPIKey's per-form readers (the gate uses the same)
	"internal/api/listener_gate.go":       2, // the gate: every value of both headers
	"internal/api/cost_attribution.go":    2, // presence only, for a metric; reads no key
	"internal/approval/approval.go":       2, // presence only, the CSRF decision; reads no key
	"internal/api/companion_artifacts.go": 1, // a signed artifact capability, not admin-class (SaaS design 2.2)
	"internal/broker/mcpserver.go":        1, // the separate broker daemon's own key, not this daemon's listener
}

var credentialReaderRe = regexp.MustCompile(`Header\.(Get|Values)\("(Authorization|X-API-Key|X-Api-Key)"\)|\.BasicAuth\(\)|Query\(\)\.Get\("(api_key|token|key|access_token)"\)`)

func TestManagedListener560_KeyReaderCensus(t *testing.T) {
	compareCensus(t, "credential reader", censusCount(t, credentialReaderRe, nil), credentialReaders)
}

// The closure test (B2a): the admin-only families grant on "admin" or an
// admin key only; none of their files may name tenant_admin.
var adminOnlyFamilyFiles = []string{
	// requireAdminGate's definition and its call sites' files.
	"internal/api/cpc_handlers.go",
	"internal/api/admin_chat_audit_handler.go",
	"internal/api/admin_handlers.go",
	"internal/api/admin_workflow_proposals_handler.go",
	"internal/api/admin_workflow_stats_handler.go",
	"internal/api/blackbox_handlers.go",
	"internal/api/doctor_ack_handler.go",
	"internal/api/featuredoctor_enable_handler.go",
	"internal/api/healing_candidates_handlers.go",
	"internal/api/healing_overrides_handlers.go",
	"internal/api/healing_triggers_handlers.go",
	"internal/api/instinct_handlers.go",
	"internal/api/leader_lock_release.go",
	"internal/api/mcp_servers_handler.go",
	"internal/api/memory_handlers.go",
	"internal/api/project_doctor_handlers.go",
	"internal/api/project_templates_handlers.go",
	"internal/api/support_report_handlers.go",
	// The admin-only routes that had their own gate until GitHub #503.
	"internal/api/admin_companion_agent_grant.go",
	"internal/api/operator_journal_handler.go",
	"internal/api/inflight.go",
	// The admin UI gate.
	"internal/admin/middleware.go",
	"internal/ui/admin.go",
}

func TestManagedListener560_AdminOnlyFamiliesGrantNoTenantAdmin(t *testing.T) {
	root, _ := moduleRootAndPath(t)
	examined := 0
	for _, f := range adminOnlyFamilyFiles {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue // absent in this tree
		}
		examined++
		if strings.Contains(string(b), "RoleTenantAdmin") || strings.Contains(string(b), `"tenant_admin"`) {
			t.Errorf("%s names tenant_admin: the admin-only families grant on admin only (M1b B2a)", f)
		}
	}
	// The admin UI gate's session-admin checker must stay the plain admin
	// comparison.
	if b, err := os.ReadFile(filepath.Join(root, "internal/service/container_http.go")); err == nil {
		src := string(b)
		i := strings.Index(src, "admin.Middleware(")
		if i < 0 {
			t.Fatal("the admin UI gate (admin.Middleware) is not in container_http.go")
		}
		end := strings.Index(src[i:], `"/admin",`)
		if end < 0 || strings.Contains(src[i:i+end], "TenantAdmin") {
			t.Error("the admin UI gate's checkers name tenant_admin or moved (M1b B2a)")
		}
		examined++
	}
	if examined < 10 {
		t.Fatalf("examined %d admin-only family file(s): the list no longer matches the tree", examined)
	}
	t.Logf("admin-only closure: %d file(s) examined", examined)
}
