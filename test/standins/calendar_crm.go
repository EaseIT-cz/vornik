package standins

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one calendar event of a stand-in user.
type Event struct {
	ID          string
	Title       string
	Location    string
	Description string
	Link        string
	Start, End  time.Time
	Attendees   int
}

// CalendarStore holds per-user events.
type CalendarStore struct {
	mu     sync.Mutex
	events map[string][]Event
	seq    int
}

// NewCalendarStore builds an empty store.
func NewCalendarStore() *CalendarStore { return &CalendarStore{events: map[string][]Event{}} }

// AddEvent adds an event for user and returns its id (assigned when empty).
func (c *CalendarStore) AddEvent(user string, e Event) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.ID == "" {
		c.seq++
		e.ID = "ev-" + strconv.Itoa(c.seq)
	}
	c.events[user] = append(c.events[user], e)
	return e.ID
}

// Events returns a copy of user's events in start order.
func (c *CalendarStore) Events(user string) []Event {
	c.mu.Lock()
	out := append([]Event(nil), c.events[user]...)
	c.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// window is the [from, to) a calendar_list_events call asks for.
func window(now time.Time, rng, tMin, tMax string) (time.Time, time.Time, error) {
	if tMin != "" || tMax != "" {
		from, err1 := time.Parse(time.RFC3339, tMin)
		to, err2 := time.Parse(time.RFC3339, tMax)
		if err1 != nil || err2 != nil || !to.After(from) {
			return time.Time{}, time.Time{}, errors.New("invalid_argument: timeMin and timeMax must be RFC 3339, timeMax after timeMin")
		}
		return from, to, nil
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch rng {
	case "", "today":
		return day, day.Add(24 * time.Hour), nil
	case "tomorrow":
		return day.Add(24 * time.Hour), day.Add(48 * time.Hour), nil
	case "week":
		return day, day.Add(7 * 24 * time.Hour), nil
	}
	return time.Time{}, time.Time{}, errors.New("invalid_argument: range must be today, tomorrow or week")
}

// NewCalendarMCP serves store as the calendar MCP server the agenda and
// morning-brief recipes call: calendar_list_events, and the write
// calendar_create_event. now is the clock the ranges are measured against.
func NewCalendarMCP(store *CalendarStore, now func() time.Time) *MCPServer {
	list := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			Range   string `json:"range"`
			TimeMin string `json:"timeMin"`
			TimeMax string `json:"timeMax"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		from, to, err := window(now().UTC(), in.Range, in.TimeMin, in.TimeMax)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, e := range store.Events(user) {
			if e.Start.Before(to) && e.End.After(from) {
				out = append(out, map[string]any{"id": e.ID, "summary": e.Title, "start": e.Start.UTC().Format(time.RFC3339),
					"end": e.End.UTC().Format(time.RFC3339), "location": e.Location, "description": e.Description,
					"hangoutLink": e.Link, "attendees": e.Attendees})
			}
		}
		return map[string]any{"events": out}, nil
	}
	create := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			Title    string `json:"title"`
			Start    string `json:"start"`
			End      string `json:"end"`
			Location string `json:"location"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		start, err1 := time.Parse(time.RFC3339, in.Start)
		end, err2 := time.Parse(time.RFC3339, in.End)
		if err1 != nil || err2 != nil || !end.After(start) || strings.TrimSpace(in.Title) == "" {
			return nil, errors.New("invalid_argument: title, and start before end in RFC 3339")
		}
		id := store.AddEvent(user, Event{Title: in.Title, Location: in.Location, Start: start, End: end})
		return map[string]any{"id": id, "created": true}, nil
	}
	str := map[string]any{"type": "string"}
	return NewMCPServer("calendar",
		Tool{Name: "calendar_list_events", Description: "List events in a range.", Handle: list,
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"range": map[string]any{"enum": []string{"today", "tomorrow", "week"}}, "timeMin": str, "timeMax": str}}},
		Tool{Name: "calendar_create_event", Description: "Create an event.", Write: true, Handle: create,
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"title", "start", "end"},
				"properties": map[string]any{"title": str, "start": str, "end": str, "location": str}}},
	)
}

// Contact is one CRM contact of a stand-in user.
type Contact struct {
	ID      string
	Name    string
	Company string
	Email   string
	Notes   []string
}

// CRMStore holds per-user contacts.
type CRMStore struct {
	mu       sync.Mutex
	contacts map[string][]*Contact
	seq      int
}

// NewCRMStore builds an empty store.
func NewCRMStore() *CRMStore { return &CRMStore{contacts: map[string][]*Contact{}} }

// AddContact adds a contact for user and returns its id.
func (c *CRMStore) AddContact(user string, ct Contact) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ct.ID == "" {
		c.seq++
		ct.ID = "c-" + strconv.Itoa(c.seq)
	}
	c.contacts[user] = append(c.contacts[user], &ct)
	return ct.ID
}

// Contact returns a copy of user's contact id.
func (c *CRMStore) Contact(user, id string) (Contact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ct := range c.contacts[user] {
		if ct.ID == id {
			cp := *ct
			cp.Notes = append([]string(nil), ct.Notes...)
			return cp, true
		}
	}
	return Contact{}, false
}

// NewCRMMCP serves store as a generic CRM: crm_search_contacts and
// crm_get_contact read; crm_add_note writes.
func NewCRMMCP(store *CRMStore) *MCPServer {
	search := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			Query string `json:"query"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		q := strings.ToLower(in.Query)
		out := []map[string]any{}
		store.mu.Lock()
		for _, ct := range store.contacts[user] {
			if strings.Contains(strings.ToLower(ct.Name+" "+ct.Company+" "+ct.Email), q) {
				out = append(out, map[string]any{"id": ct.ID, "name": ct.Name, "company": ct.Company})
			}
		}
		store.mu.Unlock()
		return map[string]any{"contacts": out}, nil
	}
	get := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		ct, ok := store.Contact(user, in.ID)
		if !ok {
			return nil, errors.New("not_found: no contact " + strconv.Quote(in.ID))
		}
		return map[string]any{"id": ct.ID, "name": ct.Name, "company": ct.Company, "email": ct.Email, "notes": ct.Notes}, nil
	}
	note := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			ContactID string `json:"contact_id"`
			Note      string `json:"note"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, ct := range store.contacts[user] {
			if ct.ID == in.ContactID && strings.TrimSpace(in.Note) != "" {
				ct.Notes = append(ct.Notes, in.Note)
				return map[string]any{"added": true, "notes": len(ct.Notes)}, nil
			}
		}
		return nil, errors.New("not_found: no contact " + strconv.Quote(in.ContactID))
	}
	str := map[string]any{"type": "string"}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	return NewMCPServer("crm",
		Tool{Name: "crm_search_contacts", Description: "Search contacts.", Handle: search, InputSchema: obj(map[string]any{"query": str}, "query")},
		Tool{Name: "crm_get_contact", Description: "Read one contact.", Handle: get, InputSchema: obj(map[string]any{"id": str}, "id")},
		Tool{Name: "crm_add_note", Description: "Add a note to a contact.", Write: true, Handle: note,
			InputSchema: obj(map[string]any{"contact_id": str, "note": str}, "contact_id", "note")},
	)
}
