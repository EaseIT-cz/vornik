package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// The session-role census (GitHub #560; 18-security-and-tenancy.md,
// "Amendment 2026-10-28 (#560, slice M1a)", L2a). Managed hosting demotes an
// admin's session to the role user on every listener but the operations one
// (A4b), which is safe only because every reader of the session role compares
// it in one of two styles: == "admin" grants, == "user" restricts. M1b adds a
// third role (tenant_admin) by editing each of those readers. This census pins
// the readers and the one writer, so a new one fails here until it is placed
// in L2a's table and here, and M1b's edit set cannot drift. Files absent from
// the tree (the Community export) are skipped.
var sessionRoleReaders = map[string]int{
	"internal/api/admin_class_auth.go":   2, // + the tenant_admin branch of OperatorSurfaceAccessFor (M1b)
	"internal/api/reminders_handlers.go": 1,
	"internal/api/doctor_ack_handler.go": 1,
	"internal/api/cpc_handlers.go":       1,
	"internal/ui/operator_accounts.go":   1,
	"internal/ui/task_conversation.go":   2,
	"internal/service/container_http.go": 1,
	"internal/ui/setup.go":               1,
	"internal/ui/server.go":              1,
	"internal/ui/projects_new.go":        2,
	"internal/ui/projects_new_wizard.go": 1,
	"internal/ui/story.go":               1,
	"internal/ui/composer_preview.go":    1,
	"internal/ui/dashboard.go":           2,
	"internal/ui/inbox.go":               1,
	"internal/api/surface.go":            1, // logTenantAdminDecision (M1b)
	"internal/api/owner_export_581.go":   1, // the owner export: tenant_admin only (#581 D5)
	"internal/ui/companion_page_587.go":  1, // the companion page: tenant_admin only (#587)
	"internal/ui/project_keys.go":        1, // logs the owner's keys-page decision (#754); decides nothing
	"internal/ui/manage.go":              1, // the Manage overview shows the owner its cards (#759); a view, not a gate
}

// sessionRoleWriters write auth.ExtraSessionRole into an identity.
var sessionRoleWriters = map[string]int{
	"internal/authsession/backend.go": 1,
}

var (
	sessionRoleReadRe  = regexp.MustCompile(`SessionRoleFromContext\(`)
	sessionRoleWriteRe = regexp.MustCompile(`auth\.ExtraSessionRole\s*:`)
)

func TestSessionRoleCensus560(t *testing.T) {
	root, _ := moduleRootAndPath(t)
	files := nonTestGoFiles(t)
	check := func(what string, re *regexp.Regexp, want map[string]int, skip map[string]bool) {
		got := map[string]int{}
		for _, f := range files {
			if skip[f] {
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
		var bad []string
		for f, n := range got {
			if want[f] != n {
				bad = append(bad, f)
			}
		}
		for f := range want {
			if _, err := os.Stat(filepath.Join(root, f)); err != nil {
				continue // absent in this tree (the Community export)
			}
			if got[f] != want[f] {
				bad = append(bad, f)
			}
		}
		sort.Strings(bad)
		for _, f := range dedupe(bad) {
			t.Errorf("%s: %s has %d site(s), the census lists %d: place it in 18-security-and-tenancy.md L2a and here", what, f, got[f], want[f])
		}
		t.Logf("%s census: %d file(s) examined, %d with a site", what, len(files), len(got))
	}
	// The reader's own definition is not a call site.
	check("session-role reader", sessionRoleReadRe, sessionRoleReaders, map[string]bool{"internal/api/middleware.go": true})
	check("session-role writer", sessionRoleWriteRe, sessionRoleWriters, nil)
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
