package approverdevice

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// GitHub #413 (2026-10-17): an approved read tool was never re-judged. The
// daemon now suspends an agent read tool whose server says it changes
// things (agent-administered design §6.5); the person sees it on the
// Approvals page, from the daemon's lines, escaped like every other text.
func TestPages_ListShowsWithheldTools(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.svc.SetWithheldTools(func(context.Context) []string {
		return []string{"Withheld: hermes's server mail in comms. Vornik stopped <b>search_messages</b>."}
	})
	res, body := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if res.StatusCode != 200 || !strings.Contains(body, "Vornik stopped &lt;b&gt;search_messages&lt;/b&gt;.") {
		t.Fatalf("list: %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "Nothing is waiting") {
		t.Fatal("a withheld line is not a decision: the empty-list text must stay")
	}
}
