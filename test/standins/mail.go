package standins

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Message is one mail in a stand-in mailbox.
type Message struct {
	ID       string
	From     string
	To       string
	Subject  string
	Body     string
	Received time.Time
	Unread   bool
}

// MailStore holds per-user mailboxes (INBOX and Sent). The Gmail-shaped MCP
// server, the IMAP server and the SMTP sink all serve the same store.
type MailStore struct {
	mu      sync.Mutex
	inbox   map[string][]Message
	sent    map[string][]Message
	address map[string]string // user -> address
	byAddr  map[string]string // address -> user
	seq     int
	imap    *IMAPServer // set by StartIMAP; deliveries are appended there too
}

// NewMailStore builds an empty store.
func NewMailStore() *MailStore {
	return &MailStore{inbox: map[string][]Message{}, sent: map[string][]Message{}, address: map[string]string{}, byAddr: map[string]string{}}
}

// AddUser adds a user and the address their mail is sent from and to.
func (m *MailStore) AddUser(user, address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address[user] = address
	m.byAddr[strings.ToLower(address)] = user
	if _, ok := m.inbox[user]; !ok {
		m.inbox[user] = nil
	}
}

// Address is user's address.
func (m *MailStore) Address(user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.address[user]
}

func (m *MailStore) nextIDLocked() string {
	m.seq++
	return "m-" + strconv.Itoa(m.seq)
}

// Deliver puts msg into user's INBOX and returns its id (assigned when
// empty). The To field defaults to the user's address.
func (m *MailStore) Deliver(user string, msg Message) string {
	m.mu.Lock()
	if msg.ID == "" {
		msg.ID = m.nextIDLocked()
	}
	if msg.To == "" {
		msg.To = m.address[user]
	}
	if msg.Received.IsZero() {
		msg.Received = time.Now().UTC()
	}
	m.inbox[user] = append(m.inbox[user], msg)
	srv := m.imap
	m.mu.Unlock()
	if srv != nil {
		srv.append(user, "INBOX", msg)
	}
	return msg.ID
}

// send records msg in from's Sent and delivers it to a local recipient.
func (m *MailStore) send(fromUser string, msg Message) string {
	m.mu.Lock()
	msg.ID = m.nextIDLocked()
	if msg.From == "" {
		msg.From = m.address[fromUser]
	}
	m.sent[fromUser] = append(m.sent[fromUser], msg)
	to, local := m.byAddr[strings.ToLower(msg.To)]
	m.mu.Unlock()
	if local {
		in := msg
		in.ID, in.Unread = "", true
		m.Deliver(to, in)
	}
	return msg.ID
}

// Inbox returns a copy of user's INBOX in delivery order.
func (m *MailStore) Inbox(user string) []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.inbox[user]...)
}

// Sent returns a copy of user's Sent.
func (m *MailStore) Sent(user string) []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.sent[user]...)
}

// matches reports whether msg matches a query of the supported subset:
// newer_than:<n>d|h, is:unread, from:<text>, and free words (all must occur
// in from, subject or body, case-insensitively).
func matches(msg Message, query string, now time.Time) (bool, error) {
	for _, term := range strings.Fields(query) {
		lower := strings.ToLower(term)
		switch {
		case strings.HasPrefix(lower, "newer_than:"):
			spec := strings.TrimPrefix(lower, "newer_than:")
			if len(spec) < 2 {
				return false, fmt.Errorf("newer_than: %q", spec)
			}
			n, err := strconv.Atoi(spec[:len(spec)-1])
			if err != nil || n < 0 {
				return false, fmt.Errorf("newer_than: %q", spec)
			}
			var d time.Duration
			switch spec[len(spec)-1] {
			case 'd':
				d = time.Duration(n) * 24 * time.Hour
			case 'h':
				d = time.Duration(n) * time.Hour
			default:
				return false, fmt.Errorf("newer_than: %q (d or h only)", spec)
			}
			if msg.Received.Before(now.Add(-d)) {
				return false, nil
			}
		case lower == "is:unread":
			if !msg.Unread {
				return false, nil
			}
		case strings.HasPrefix(lower, "from:"):
			if !strings.Contains(strings.ToLower(msg.From), strings.TrimPrefix(lower, "from:")) {
				return false, nil
			}
		default:
			hay := strings.ToLower(msg.From + " " + msg.Subject + " " + msg.Body)
			if !strings.Contains(hay, lower) {
				return false, nil
			}
		}
	}
	return true, nil
}

// NewMailMCP serves store as a Gmail-shaped MCP server with the reference
// Google Workspace server's read tools, by their names and argument names
// (gemini-cli workspace extension 089927e, workspace-server/src/index.ts;
// GitHub #404): gmail_search {query, maxResults, pageToken, labelIds,
// includeSpamTrash} and gmail_get {messageId, format}; and the stand-in's
// own write gmail_send, which imitates no reference shape. now is the clock
// newer_than is measured against.
func NewMailMCP(store *MailStore, now func() time.Time) *MCPServer {
	search := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			Query            string   `json:"query"`
			MaxResults       int      `json:"maxResults"`
			PageToken        string   `json:"pageToken"`
			LabelIDs         []string `json:"labelIds"`
			IncludeSpamTrash bool     `json:"includeSpamTrash"` // nothing is spam or trash here
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.MaxResults < 0 {
			return nil, errors.New("invalid_argument: maxResults must not be negative")
		}
		if in.PageToken != "" {
			return nil, errors.New("invalid_argument: unknown pageToken (the stand-in returns every match on one page)")
		}
		unreadOnly := false
		for _, l := range in.LabelIDs {
			switch l {
			case "INBOX":
			case "UNREAD":
				unreadOnly = true
			default:
				return nil, errors.New("invalid_argument: label " + strconv.Quote(l) + " is not imitated (INBOX and UNREAD are)")
			}
		}
		msgs := store.Inbox(user)
		sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Received.After(msgs[j].Received) })
		out := []map[string]any{}
		for _, m := range msgs {
			ok, err := matches(m, in.Query, now())
			if err != nil {
				return nil, err
			}
			if ok && (!unreadOnly || m.Unread) {
				out = append(out, map[string]any{"id": m.ID, "from": m.From, "subject": m.Subject, "received_at": m.Received.UTC().Format(time.RFC3339)})
			}
			if in.MaxResults > 0 && len(out) == in.MaxResults {
				break
			}
		}
		return map[string]any{"messages": out}, nil
	}
	get := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			MessageID string `json:"messageId"`
			Format    string `json:"format"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if in.MessageID == "" {
			return nil, errors.New("invalid_argument: messageId is required")
		}
		withBody := true
		switch in.Format {
		case "", "full", "raw": // raw is not RFC 2822 bytes here
		case "minimal", "metadata":
			withBody = false
		default:
			return nil, errors.New("invalid_argument: format must be minimal, full, raw or metadata")
		}
		for _, m := range store.Inbox(user) {
			if m.ID == in.MessageID {
				out := map[string]any{"id": m.ID, "from": m.From, "to": m.To, "subject": m.Subject,
					"received_at": m.Received.UTC().Format(time.RFC3339), "unread": m.Unread}
				if withBody {
					out["body"] = m.Body
				}
				return out, nil
			}
		}
		return nil, errors.New("not_found: no message " + strconv.Quote(in.MessageID))
	}
	send := func(user string, args json.RawMessage) (any, error) {
		var in struct {
			To        string `json:"to"`
			Subject   string `json:"subject"`
			Body      string `json:"body"`
			InReplyTo string `json:"in_reply_to"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		if !strings.Contains(in.To, "@") {
			return nil, errors.New("invalid_argument: to must be an address")
		}
		id := store.send(user, Message{To: in.To, Subject: in.Subject, Body: in.Body, Received: now().UTC()})
		return map[string]any{"sent": true, "id": id}, nil
	}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	str := map[string]any{"type": "string"}
	return NewMCPServer("mail",
		Tool{Name: "gmail_search", Description: "Search the mailbox.", Handle: search,
			InputSchema: obj(map[string]any{"query": str, "maxResults": map[string]any{"type": "integer", "minimum": 0},
				"pageToken": str, "labelIds": map[string]any{"type": "array", "items": str},
				"includeSpamTrash": map[string]any{"type": "boolean"}})},
		Tool{Name: "gmail_get", Description: "Read one message.", Handle: get, InputSchema: obj(map[string]any{"messageId": str,
			"format": map[string]any{"enum": []string{"minimal", "full", "raw", "metadata"}}}, "messageId")},
		Tool{Name: "gmail_send", Description: "Send a message.", Write: true, Handle: send,
			InputSchema: obj(map[string]any{"to": str, "subject": str, "body": str, "in_reply_to": str}, "to")},
	)
}
