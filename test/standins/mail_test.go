package standins

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GitHub #337 (2026-10-12): one mail store, several users; the bearer
// chooses whose mailbox a tool reads, and no tool reaches another user's.

var clock = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return clock }

func seededMail() *MailStore {
	m := NewMailStore()
	m.AddUser("alice", "alice@home.example")
	m.AddUser("bob", "bob@work.example")
	m.Deliver("alice", Message{ID: "a-1", From: "billing@acme.example", Subject: "Invoice 4411", Body: "Pay 4411. BODY-CANARY-A", Received: clock.Add(-2 * time.Hour), Unread: true})
	m.Deliver("alice", Message{ID: "a-2", From: "news@letters.example", Subject: "Weekly news", Body: "Old news.", Received: clock.Add(-72 * time.Hour)})
	m.Deliver("bob", Message{ID: "b-1", From: "boss@work.example", Subject: "Quarterly plan", Body: "BODY-CANARY-B", Received: clock.Add(-1 * time.Hour), Unread: true})
	return m
}

type searchResult struct {
	Messages []struct {
		ID, From, Subject string
		ReceivedAt        string `json:"received_at"`
	} `json:"messages"`
}

func ids(r searchResult) string {
	var out []string
	for _, m := range r.Messages {
		out = append(out, m.ID)
	}
	return strings.Join(out, ",")
}

func TestMailMCP_SearchAndGetArePerUser(t *testing.T) {
	store := seededMail()
	s := NewMailMCP(store, fixedNow)
	s.Token("tok-alice", "alice")
	s.Token("tok-bob", "bob")
	srv := httptest.NewServer(s)
	defer srv.Close()
	alice := dial(t, srv.URL, "tok-alice")

	cases := []struct{ query, want string }{
		{"", "a-1,a-2"},
		{"newer_than:1d", "a-1"},
		{"newer_than:5h", "a-1"},
		{"newer_than:1h", ""},
		{"is:unread", "a-1"},
		{"from:letters.example", "a-2"},
		{"invoice", "a-1"},
		{"quarterly", ""}, // bob's message: never alice's result
	}
	for _, c := range cases {
		var r searchResult
		call(t, alice, "gmail_search", map[string]any{"query": c.query}, &r)
		if got := ids(r); got != c.want {
			t.Errorf("search %q = %q, want %q", c.query, got, c.want)
		}
	}

	var msg struct{ ID, Body string }
	call(t, alice, "gmail_get", map[string]any{"messageId": "a-1"}, &msg)
	if !strings.Contains(msg.Body, "BODY-CANARY-A") {
		t.Fatalf("get a-1: %+v", msg)
	}
	if text, isErr := call(t, alice, "gmail_get", map[string]any{"messageId": "b-1"}, nil); !isErr || strings.Contains(text, "CANARY") {
		t.Fatalf("alice read bob's message: %s", text)
	}
	var limited searchResult
	call(t, alice, "gmail_search", map[string]any{"query": "", "maxResults": 1}, &limited)
	if ids(limited) != "a-1" {
		t.Fatalf("maxResults 1 = %s (newest first)", ids(limited))
	}
	for _, c := range s.Calls() {
		if c.User != "alice" {
			t.Fatalf("a call recorded for %s", c.User)
		}
	}
}

func TestMailMCP_SendDeliversLocallyAndIsAWrite(t *testing.T) {
	store := seededMail()
	s := NewMailMCP(store, fixedNow)
	s.Token("tok-alice", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()

	var sent struct {
		Sent bool
		ID   string
	}
	call(t, dial(t, srv.URL, "tok-alice"), "gmail_send", map[string]any{"to": "bob@work.example", "subject": "Re: plan", "body": "Fine."}, &sent)
	if !sent.Sent || sent.ID == "" {
		t.Fatalf("send: %+v", sent)
	}
	if out := store.Sent("alice"); len(out) != 1 || out[0].To != "bob@work.example" {
		t.Fatalf("alice's Sent = %+v", out)
	}
	in := store.Inbox("bob")
	if len(in) != 2 || in[len(in)-1].From != "alice@home.example" || !in[len(in)-1].Unread {
		t.Fatalf("bob's INBOX = %+v", in)
	}
	if len(s.Writes()) != 1 {
		t.Fatalf("writes = %d", len(s.Writes()))
	}
	// A recipient outside the store is accepted and only recorded as sent.
	call(t, dial(t, srv.URL, "tok-alice"), "gmail_send", map[string]any{"to": "x@elsewhere.example", "subject": "s", "body": "b"}, nil)
	if len(store.Sent("alice")) != 2 {
		t.Fatal("an external send was not recorded")
	}
	if text, isErr := call(t, dial(t, srv.URL, "tok-alice"), "gmail_send", map[string]any{"subject": "no recipient"}, nil); !isErr {
		t.Fatalf("a send without to: %s", text)
	}
}

// GitHub #404 (2026-10-17): the stand-in takes the reference Google
// Workspace server's argument names (gemini-cli workspace extension,
// 089927e, workspace-server/src/index.ts) and refuses the ones it used to
// take; an argument it does not imitate is refused, never ignored.
func TestMailMCP_TakesTheReferenceArgumentNames(t *testing.T) {
	s := NewMailMCP(seededMail(), fixedNow)
	s.Token("tok-alice", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()
	alice := dial(t, srv.URL, "tok-alice")

	ok := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"query": "", "maxResults": 1, "includeSpamTrash": true}, "a-1"},
		{map[string]any{"labelIds": []string{"INBOX"}}, "a-1,a-2"},
		{map[string]any{"labelIds": []string{"INBOX", "UNREAD"}}, "a-1"},
		{map[string]any{"query": "newer_than:5d", "pageToken": ""}, "a-1,a-2"},
	}
	for _, c := range ok {
		var r searchResult
		if text, isErr := call(t, alice, "gmail_search", c.args, &r); isErr {
			t.Fatalf("gmail_search %v refused: %s", c.args, text)
		}
		if got := ids(r); got != c.want {
			t.Errorf("gmail_search %v = %q, want %q", c.args, got, c.want)
		}
	}
	for _, args := range []map[string]any{
		{"max_results": 1},             // the stand-in's old name
		{"labelIds": []string{"SPAM"}}, // a label it does not imitate
		{"pageToken": "next-page"},     // it never pages
		{"maxResults": -1},
	} {
		if text, isErr := call(t, alice, "gmail_search", args, nil); !isErr {
			t.Errorf("gmail_search %v accepted: %s", args, text)
		}
	}

	var full struct{ ID, Body string }
	call(t, alice, "gmail_get", map[string]any{"messageId": "a-1", "format": "full"}, &full)
	if full.ID != "a-1" || !strings.Contains(full.Body, "BODY-CANARY-A") {
		t.Fatalf("format full = %+v", full)
	}
	for _, f := range []string{"metadata", "minimal"} {
		text, isErr := call(t, alice, "gmail_get", map[string]any{"messageId": "a-1", "format": f}, nil)
		if isErr || strings.Contains(text, "BODY-CANARY-A") || !strings.Contains(text, "a-1") {
			t.Errorf("format %s = %s (no body expected)", f, text)
		}
	}
	for _, args := range []map[string]any{
		{"id": "a-1"}, // the stand-in's old name
		{},            // messageId is required
		{"messageId": "a-1", "format": "html"},
	} {
		if text, isErr := call(t, alice, "gmail_get", args, nil); !isErr {
			t.Errorf("gmail_get %v accepted: %s", args, text)
		}
	}
	if n := len(s.Failed()); n != 7 {
		t.Fatalf("Failed() = %d calls, want the 7 refused ones", n)
	}
	for _, c := range s.Failed() {
		if c.Err == "" {
			t.Fatalf("a failed call without its error: %+v", c)
		}
	}
}
