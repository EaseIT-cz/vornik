package ui

// The Manage overview (ui-design-refresh-design.md §17.2 M1, #759). Before
// it the Manage door landed on the assistants list, and nothing said what
// else Manage holds; which entries a viewer saw was decided in the browser.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
)

// manageAs renders /ui/manage with the given request context and returns
// the card keys and their links, from the server output (no script).
func manageAs(t *testing.T, srv *Server, req *http.Request, trading bool) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.serveManage(rec, req, func() bool { return trading })
	if rec.Code != http.StatusOK {
		t.Fatalf("manage: %d", rec.Code)
	}
	out := map[string]string{}
	card := regexp.MustCompile(`<a href="([^"]+)" class="v-card[^"]*" data-manage-card="([^"]+)"`)
	for _, m := range card.FindAllStringSubmatch(rec.Body.String(), -1) {
		out[m[2]] = m[1]
	}
	return out
}

func manageKeys759(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func sessionReq(t *testing.T, b keysSessionBackend) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/manage", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: b})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

func TestManage759_EachAudienceSeesItsCards(t *testing.T) {
	srv := NewServer()

	user := manageAs(t, srv, sessionReq(t, member754), false)
	if got := strings.Join(manageKeys759(user), ","); got != "insights,my-account,projects,reminders" {
		t.Errorf("project user sees %s", got)
	}
	owner := manageAs(t, srv, sessionReq(t, owner754), false)
	if got := strings.Join(manageKeys759(owner), ","); got != "admin-keys,insights,my-account,projects,reminders" {
		t.Errorf("owner sees %s", got)
	}
	if owner["admin-keys"] != "/ui/credentials" {
		t.Errorf("the owner's Keys & access goes to %q, want /ui/credentials", owner["admin-keys"])
	}
	adminCards := manageAs(t, srv, withAdminUI(httptest.NewRequest(http.MethodGet, "/manage", nil)), false)
	if len(adminCards) != 8 || adminCards["admin-keys"] != "/ui/admin/keys" {
		t.Errorf("admin sees %v (want the eight non-trading cards, Keys at /ui/admin/keys)", manageKeys759(adminCards))
	}
	if _, ok := manageAs(t, srv, withAdminUI(httptest.NewRequest(http.MethodGet, "/manage", nil)), true)["trading"]; !ok {
		t.Error("with the trading capability on, the admin sees Trading")
	}
	if _, ok := manageAs(t, srv, sessionReq(t, member754), true)["trading"]; !ok {
		t.Error("Trading is not admin-only: a project user sees it when the capability is on")
	}
	// Every card links to its nav destination (the owner's Keys & access,
	// checked above, is the one OwnerHref that differs).
	for _, a := range navModel() {
		for _, d := range a.Dests {
			if href, ok := user[d.Key]; ok && href != d.Href {
				t.Errorf("%s links to %q, want %q", d.Key, href, d.Href)
			}
		}
	}
	// Every card says what its page is for.
	for k := range adminCards {
		if managePurpose[k] == "" {
			t.Errorf("card %q has no purpose line", k)
		}
	}
}

func TestNav_759_ManageLandsOnItsOverview(t *testing.T) {
	for _, a := range navModel() {
		if a.Key == "manage" && (a.Href != "/ui/manage" || len(a.Dests) == 0 || a.Dests[0].Key != "manage-overview") {
			t.Errorf("Manage should land on /ui/manage with Overview first: %+v", a)
		}
	}
	if navAreaForPage("manage-overview") != "manage" {
		t.Error("pageToArea should map the overview to Manage")
	}
}
