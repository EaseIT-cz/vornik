package hermes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDevice mimics the device routes: pairing sets the cookie, the list and
// request pages render as the real templates do, and a decision must echo
// the rendered sha256 and carry the cookie and same-origin headers.
type fakeDevice struct {
	mu        sync.Mutex
	decisions map[string]string // id -> "approve:<value>" | "reject"
}

func (f *fakeDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/ui/pair" {
		if r.FormValue("code") != "ABCD2345" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "vornik_device", Value: "dev-cookie", Path: "/"})
		http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie("vornik_device"); err != nil || c.Value != "dev-cookie" {
		http.Error(w, "no device", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ui/approve/":
		// The list as b9a2db54b draws it: a group card first, and each
		// Review link a link-button (class="act"). The scripted phone read
		// none of these cards once the class attribute appeared (found by the
		// broker design §18 E2E, 2026-10-03).
		_, _ = fmt.Fprint(w, `<div class="card"><p>2 waiting: writes</p><a class="act" href="/ui/approve/group/w">Review together</a></div>`+
			`<div class="card"><p>Connect the bank API &#34;fio&#34;.</p><p class="mut">now</p><a class="act" href="/ui/approve/apr_1">Review</a></div>`+
			`<div class="card"><p>Enter BANK_KEY.</p><p class="mut">now</p><a class="act" href="/ui/approve/apr_2">Review</a></div>`)
	case r.URL.Path != "/ui/approve/apr_1" && r.URL.Path != "/ui/approve/apr_2":
		http.NotFound(w, r)
	case r.Method == http.MethodGet:
		_, _ = fmt.Fprintf(w, `<form method="post" action="%s"><input type="hidden" name="rendered_sha256" value="sha-%s"></form>`, r.URL.Path, strings.TrimPrefix(r.URL.Path, "/ui/approve/"))
	case r.Method == http.MethodPost:
		id := strings.TrimPrefix(r.URL.Path, "/ui/approve/")
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" || r.PostFormValue("rendered_sha256") != "sha-"+id {
			http.Error(w, "stale or cross-site", http.StatusConflict)
			return
		}
		f.decisions[id] = r.PostFormValue("decision") + ":" + r.PostFormValue("value") + r.PostFormValue("grant_days") + r.PostFormValue("grant_uses")
		http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

// Agent-administered Vornik plan P8.1: the scripted phone pairs with the
// printed code, lists pending requests with their sentences, and decides
// one by echoing the rendered sha256 it was shown, with the value for a
// credential. Control: Phone.
func TestPhone(t *testing.T) {
	dev := &fakeDevice{decisions: map[string]string{}}
	srv := httptest.NewServer(dev)
	defer srv.Close()
	p := NewPhone(srv.URL)
	if _, err := p.Pending(); err == nil {
		t.Fatal("listed before pairing")
	}
	if err := p.Pair("WRONG000"); err == nil {
		t.Fatal("paired with a wrong code")
	}
	if err := p.Pair("ABCD2345"); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Pending()
	if err != nil || len(pending) != 2 || pending[0].ID != "apr_1" || pending[0].Sentence != `Connect the bank API "fio".` {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if err := p.Approve("apr_1", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Approve("apr_2", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	if dev.decisions["apr_1"] != "approve:" || dev.decisions["apr_2"] != "approve:sk-secret" {
		t.Fatalf("decisions: %v", dev.decisions)
	}
	if p.Shown["apr_2"] != "sha-apr_2" {
		t.Fatalf("shown hashes: %v", p.Shown)
	}
	if err := p.Reject("apr_9"); err == nil {
		t.Fatal("decided a request that does not exist")
	}
	// Hermes approval transport design §4.2: a host action is answered with
	// a choice, not approve.
	if err := p.Answer("apr_1", "session"); err != nil {
		t.Fatal(err)
	}
	if dev.decisions["apr_1"] != "session:" {
		t.Fatalf("answer: %v", dev.decisions)
	}
	// Broker write-actions design, tier 2: approve with a standing grant
	// sends the page's grant choice.
	if err := p.ApproveWithGrant("apr_2", 7, 20); err != nil {
		t.Fatal(err)
	}
	if dev.decisions["apr_2"] != "approve_grant:720" {
		t.Fatalf("approve with a grant: %v", dev.decisions)
	}
}

// GitHub #337 (2026-10-12): Connect follows the device route's redirect to
// the provider and the provider's redirect back to the callback, carrying
// the flow cookie the device route set; a page without Connect is refused.
func TestPhoneConnect(t *testing.T) {
	var daemonURL string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/authorize" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, daemonURL+"/auth/mcp/callback?code=c1&state="+r.URL.Query().Get("state"), http.StatusFound)
	}))
	defer provider.Close()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ui/approve/apr_o":
			_, _ = fmt.Fprint(w, `<form method="post" action="/ui/approve/apr_o/connect"><input type="hidden" name="rendered_sha256" value="sha-o"></form>`)
		case r.Method == http.MethodGet && r.URL.Path == "/ui/approve/apr_x":
			_, _ = fmt.Fprint(w, `<form method="post" action="/ui/approve/apr_x"><input type="hidden" name="rendered_sha256" value="sha-x"></form>`)
		case r.Method == http.MethodPost && r.URL.Path == "/ui/approve/apr_o/connect":
			if r.PostFormValue("rendered_sha256") != "sha-o" {
				http.Error(w, "stale", http.StatusConflict)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "vornik_oauth_flow", Value: "flow-1", Path: "/auth/mcp/callback", HttpOnly: true})
			http.Redirect(w, r, provider.URL+"/authorize?state=s1", http.StatusSeeOther)
		case r.URL.Path == "/auth/mcp/callback":
			if c, err := r.Cookie("vornik_oauth_flow"); err != nil || c.Value != "flow-1" || r.URL.Query().Get("code") != "c1" {
				http.Error(w, "Open this on your phone", http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, "<h1>Connected</h1><p>Signed in as alice@home.example.</p>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	daemonURL = daemon.URL
	p := NewPhone(daemon.URL)
	page, err := p.Connect("apr_o")
	if err != nil || !strings.Contains(page, "Signed in as alice@home.example") {
		t.Fatalf("connect: %v %s", err, page)
	}
	if p.Shown["apr_o"] != "sha-o" {
		t.Fatalf("shown = %q", p.Shown["apr_o"])
	}
	if _, err := p.Connect("apr_x"); err == nil {
		t.Fatal("connected a request whose page offers no Connect")
	}
}
