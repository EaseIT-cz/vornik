package standins

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GitHub #337 (2026-10-12): the calendar answers calendar_list_events (the
// recipes' spelling) with the reference server's arguments (GitHub #404), per user, against an
// injected clock; the CRM is the generic read-and-write server.

func seededCalendar() *CalendarStore {
	c := NewCalendarStore()
	day := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	c.AddEvent("alice", Event{ID: "e-1", Title: "Standup", Start: day.Add(9 * time.Hour), End: day.Add(9*time.Hour + 30*time.Minute), Attendees: 4, Link: "https://meet.example/x", Description: "DESC-CANARY"})
	c.AddEvent("alice", Event{ID: "e-2", Title: "Dentist", Location: "Main St 1", Start: day.Add(33 * time.Hour), End: day.Add(34 * time.Hour), Attendees: 1})
	c.AddEvent("alice", Event{ID: "e-3", Title: "Offsite", Start: day.Add(5 * 24 * time.Hour), End: day.Add(5*24*time.Hour + time.Hour), Attendees: 12})
	c.AddEvent("bob", Event{ID: "e-9", Title: "Bob's review", Start: day.Add(10 * time.Hour), End: day.Add(11 * time.Hour)})
	return c
}

type eventsResult struct {
	Events []struct {
		ID, Start, End, Summary, Location string
		Attendees                         int
	} `json:"events"`
}

func eventIDs(r eventsResult) string {
	var out []string
	for _, e := range r.Events {
		out = append(out, e.ID)
	}
	return strings.Join(out, ",")
}

func TestCalendarMCP_WindowsArePerUserAndAgainstTheClock(t *testing.T) {
	s := NewCalendarMCP(seededCalendar(), fixedNow)
	s.Token("tok-alice", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()
	alice := dial(t, srv.URL, "tok-alice")

	// GitHub #404 (2026-10-17): the reference calendar.listEvents arguments,
	// calendarId required, timeMin defaulting to now, timeMax optional.
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"calendarId": "primary", "timeMin": "2026-10-12T00:00:00Z", "timeMax": "2026-10-13T00:00:00Z"}, "e-1"},
		{map[string]any{"calendarId": "primary", "timeMin": "2026-10-13T00:00:00Z", "timeMax": "2026-10-14T00:00:00Z"}, "e-2"},
		{map[string]any{"calendarId": "alice@home.example", "timeMin": "2026-10-13T00:00:00Z", "timeMax": "2026-10-20T00:00:00Z"}, "e-2,e-3"},
		{map[string]any{"calendarId": "primary"}, "e-1,e-2,e-3"},                            // from now (08:00), no upper bound
		{map[string]any{"calendarId": "primary", "timeMax": "2026-10-13T00:00:00Z"}, "e-1"}, // from now
		{map[string]any{"calendarId": "primary", "timeMin": "2026-10-14T00:00:00Z"}, "e-3"}, // no upper bound
		{map[string]any{"calendarId": "primary", "eventTypes": []string{"default"}}, "e-1,e-2,e-3"},
		{map[string]any{"calendarId": "primary", "eventTypes": []string{"focusTime"}}, ""},
		{map[string]any{"calendarId": "primary", "attendeeResponseStatus": []string{}}, "e-1,e-2,e-3"},
	} {
		var r eventsResult
		if text, isErr := call(t, alice, "calendar_list_events", c.args, &r); isErr {
			t.Fatalf("%v refused: %s", c.args, text)
		}
		if got := eventIDs(r); got != c.want {
			t.Errorf("%v = %q, want %q", c.args, got, c.want)
		}
	}
	for _, args := range []map[string]any{
		{}, // calendarId is required
		{"calendarId": ""},
		{"calendarId": "primary", "range": "today"}, // the stand-in's old name
		{"calendarId": "primary", "timeMin": "yesterday"},
		{"calendarId": "primary", "timeMin": "2026-10-13T00:00:00Z", "timeMax": "2026-10-12T00:00:00Z"},
		{"calendarId": "primary", "timeMin": "2026-10-13T00:00:00Z", "timeMax": "2026-10-13T00:00:00Z"}, // an empty window
		{"calendarId": "primary", "eventTypes": []string{"meeting"}},
		{"calendarId": "primary", "attendeeResponseStatus": []string{"accepted"}}, // not imitated
	} {
		if text, isErr := call(t, alice, "calendar_list_events", args, nil); !isErr {
			t.Errorf("%v accepted: %s", args, text)
		}
	}
	if n := len(s.Failed()); n != 8 {
		t.Fatalf("Failed() = %d calls, want the 8 refused ones", n)
	}
}

func TestCalendarMCP_CreateIsAWrite(t *testing.T) {
	store := seededCalendar()
	s := NewCalendarMCP(store, fixedNow)
	s.Token("tok-bob", "bob")
	srv := httptest.NewServer(s)
	defer srv.Close()
	var out struct{ ID string }
	call(t, dial(t, srv.URL, "tok-bob"), "calendar_create_event", map[string]any{"title": "1:1", "start": "2026-10-12T15:00:00Z", "end": "2026-10-12T15:30:00Z"}, &out)
	if out.ID == "" || len(store.Events("bob")) != 2 || len(store.Events("alice")) != 3 || len(s.Writes()) != 1 {
		t.Fatalf("create: %+v, bob %d, alice %d, writes %d", out, len(store.Events("bob")), len(store.Events("alice")), len(s.Writes()))
	}
	if _, isErr := call(t, dial(t, srv.URL, "tok-bob"), "calendar_create_event", map[string]any{"title": "x", "start": "soon"}, nil); !isErr {
		t.Fatal("a bad start was accepted")
	}
}

func TestCRMMCP_ReadsAndANoteWrite(t *testing.T) {
	store := NewCRMStore()
	store.AddContact("alice", Contact{ID: "c-1", Name: "Jana Novak", Company: "Acme", Email: "jana@acme.example"})
	store.AddContact("alice", Contact{ID: "c-2", Name: "Petr Dvorak", Company: "Globex"})
	store.AddContact("bob", Contact{ID: "c-9", Name: "Bob's contact", Company: "Acme"})
	s := NewCRMMCP(store)
	s.Token("tok-alice", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()
	alice := dial(t, srv.URL, "tok-alice")

	var found struct {
		Contacts []struct{ ID, Name, Company string } `json:"contacts"`
	}
	call(t, alice, "crm_search_contacts", map[string]any{"query": "acme"}, &found)
	if len(found.Contacts) != 1 || found.Contacts[0].ID != "c-1" {
		t.Fatalf("search acme: %+v", found)
	}
	var one struct {
		ID    string
		Notes []string
	}
	call(t, alice, "crm_get_contact", map[string]any{"id": "c-1"}, &one)
	if one.ID != "c-1" {
		t.Fatalf("get: %+v", one)
	}
	if _, isErr := call(t, alice, "crm_get_contact", map[string]any{"id": "c-9"}, nil); !isErr {
		t.Fatal("alice read bob's contact")
	}
	call(t, alice, "crm_add_note", map[string]any{"contact_id": "c-1", "note": "Called about renewal."}, nil)
	if c, _ := store.Contact("alice", "c-1"); len(c.Notes) != 1 || c.Notes[0] != "Called about renewal." {
		t.Fatalf("note: %+v", c)
	}
	// The daemon adds its own project_id to every MCP call (seen by A9's
	// first local run, 2026-10-12: the approved note was refused as an
	// unknown argument); a stand-in accepts and ignores it, and still
	// refuses any other unknown argument.
	call(t, alice, "crm_add_note", map[string]any{"contact_id": "c-2", "note": "x", "project_id": "ns--sales"}, nil)
	if c, _ := store.Contact("alice", "c-2"); len(c.Notes) != 1 {
		t.Fatalf("a call carrying the daemon's project_id was refused: %+v", c)
	}
	if _, isErr := call(t, alice, "crm_add_note", map[string]any{"contact_id": "c-2", "note": "x", "notee": "y"}, nil); !isErr {
		t.Fatal("an unknown argument was accepted")
	}
	if _, isErr := call(t, alice, "crm_add_note", map[string]any{"contact_id": "c-9", "note": "x"}, nil); !isErr {
		t.Fatal("alice wrote to bob's contact")
	}
	if w := s.Writes(); len(w) != 4 {
		t.Fatalf("writes = %d (a refused write is still a recorded attempt)", len(w))
	}
}
