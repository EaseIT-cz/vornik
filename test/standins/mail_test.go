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
	call(t, alice, "gmail_get", map[string]any{"id": "a-1"}, &msg)
	if !strings.Contains(msg.Body, "BODY-CANARY-A") {
		t.Fatalf("get a-1: %+v", msg)
	}
	if text, isErr := call(t, alice, "gmail_get", map[string]any{"id": "b-1"}, nil); !isErr || strings.Contains(text, "CANARY") {
		t.Fatalf("alice read bob's message: %s", text)
	}
	var limited searchResult
	call(t, alice, "gmail_search", map[string]any{"query": "", "max_results": 1}, &limited)
	if ids(limited) != "a-1" {
		t.Fatalf("max_results 1 = %s (newest first)", ids(limited))
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
