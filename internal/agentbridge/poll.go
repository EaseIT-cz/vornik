package agentbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// A harness takes the admin tools' schemas when its session starts, so after
// a change (a deploy, a config reload that changes the model catalogue) its
// cached schema refuses a field the daemon now accepts (agent-administered
// design §18.14 finding 2, round 2 and review be5a govern, GREEN). The
// bridge runs for the whole session, so it owns
// notifications/tools/list_changed on this transport (F4): every
// PollEvery after the harness's initialize result has been relayed it sends
// its own tools/list in the harness's session (same Mcp-Session-Id and
// MCP-Protocol-Version), walking every page, hashes the canonical JSON of
// the tools, and writes the notification when the hash differs from the
// last one. The hash is taken from the harness's own tools/list replies as
// they pass through (every page, review 20261003-195e F2), so the bridge
// never notifies about a list the harness has not seen.

// DefaultPollEvery is the poll interval the design states.
const DefaultPollEvery = 60 * time.Second

// pollTimeout bounds one poll request (round 2 F2): the relay's client
// allows minutes for a long tool call; a poll is never allowed that long.
const pollTimeout = 5 * time.Second

// maxPages bounds one poll's walk, so a server whose cursor never ends
// cannot hold the poll.
const maxPages = 64

// listChangedNotification is what the bridge writes on a change.
const listChangedNotification = `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`

// newPollClient is the poll's own client (round 2 F2), never the relay's.
func newPollClient() *http.Client { return &http.Client{Timeout: pollTimeout} }

// bridge is one run's shared state: the relay and the poll goroutine both
// write through it.
type bridge struct {
	cfg       Config
	out, errw io.Writer

	// outMu makes write the one writer on stdout (round 2 F1); errMu does
	// the same for stderr.
	outMu, errMu sync.Mutex

	mu           sync.Mutex
	session      string
	protoVersion string // the first initialize result's protocol; set means initialized (F3)
	toolsHash    string // the tools the harness has seen; "" until it listed them all
	// harnessPages holds the tools of the harness's list in progress, page by
	// page, until a page without nextCursor completes it.
	harnessPages []any
	harnessOpen  bool
	polling      bool
	pollSeq      int
}

// write compacts and validates one JSON-RPC message and writes it as one
// line. Every stdout write goes through here, the relay's replies and the
// poll's notification alike. It returns the compacted message.
func (b *bridge) write(raw []byte) ([]byte, bool) {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil || !json.Valid(buf.Bytes()) {
		return nil, false
	}
	c := append([]byte(nil), buf.Bytes()...)
	buf.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(buf.Bytes())
	return c, true
}

// logf writes one diagnostic line to stderr. Callers never pass the key.
func (b *bridge) logf(format string, args ...any) {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	_, _ = fmt.Fprintf(b.errw, format+"\n", args...)
}

func (b *bridge) sessionHeaders() (session, version string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session, b.protoVersion
}

func (b *bridge) setSession(s string) {
	if s == "" {
		return
	}
	b.mu.Lock()
	b.session = s
	b.mu.Unlock()
}

// initialized records the negotiated protocol version and reports whether
// the poll should start now. Only the first initialize result counts: a
// later initialize-shaped message rewrites nothing (review 195e F4).
func (b *bridge) initialized(version string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.polling {
		return false
	}
	b.protoVersion = version
	b.polling = true
	return true
}

// seenPage records one page of a tools/list reply relayed to the harness.
// A request without a cursor starts a list; a page without nextCursor
// completes it, and the whole list becomes what the harness holds, so the
// poll compares against it.
func (b *bridge) seenPage(reply []byte, cursor string) {
	tools, next, ok := toolsPage(reply)
	if !ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case cursor == "":
		b.harnessPages, b.harnessOpen = tools, true
	case b.harnessOpen:
		b.harnessPages = append(b.harnessPages, tools...)
	default:
		return // a later page of a list whose start the bridge did not see
	}
	if next == "" {
		b.toolsHash = hashTools(b.harnessPages)
		b.harnessPages, b.harnessOpen = nil, false
	}
}

// toolsPage is one tools/list result's tools and its nextCursor.
func toolsPage(reply []byte) ([]any, string, bool) {
	var m struct {
		Result *struct {
			Tools      json.RawMessage `json:"tools"`
			NextCursor string          `json:"nextCursor"`
		} `json:"result"`
	}
	if json.Unmarshal(reply, &m) != nil || m.Result == nil || len(m.Result.Tools) == 0 {
		return nil, "", false
	}
	var tools []any
	if json.Unmarshal(m.Result.Tools, &tools) != nil || tools == nil {
		return nil, "", false
	}
	return tools, m.Result.NextCursor, true
}

// hashTools is the sha256 of the canonical JSON of a tool list (object keys
// sorted, insignificant whitespace removed).
func hashTools(tools []any) string {
	canon, _ := json.Marshal(tools)
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// pollLoop polls until ctx ends. Polls run one at a time on this goroutine,
// so a tick that comes while one is in flight is skipped (round 2 F2).
func (b *bridge) pollLoop(ctx context.Context, hc *http.Client) {
	every := b.cfg.PollEvery
	if every <= 0 {
		every = DefaultPollEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.pollOnce(ctx, hc)
		}
	}
}

// pollOnce lists the tools in the harness's session, every page, and
// notifies on a change. A failed or slow poll logs one line and changes
// nothing; a defect is recovered, so the poll can never stop the relay.
func (b *bridge) pollOnce(ctx context.Context, hc *http.Client) {
	defer func() {
		if r := recover(); r != nil {
			b.logf("vornik bridge: tools poll: recovered from a defect; the relay goes on")
		}
	}()
	if b.cfg.pollHook != nil {
		b.cfg.pollHook()
	}
	b.mu.Lock()
	session, version, last := b.session, b.protoVersion, b.toolsHash
	b.mu.Unlock()
	if last == "" {
		return // the harness has not listed the tools yet: nothing to compare
	}
	tools, err := b.listAll(ctx, hc, session, version)
	if ctx.Err() != nil {
		return // the relay ended
	}
	if err != nil {
		b.logf("vornik bridge: tools poll: %v", err)
		return
	}
	hash := hashTools(tools)
	b.mu.Lock()
	changed := hash != b.toolsHash
	if changed {
		b.toolsHash = hash
	}
	b.mu.Unlock()
	if changed {
		_, _ = b.write([]byte(listChangedNotification))
	}
}

// listAll walks every tools/list page. Any failed page fails the walk.
func (b *bridge) listAll(ctx context.Context, hc *http.Client, session, version string) ([]any, error) {
	var all []any
	cursor := ""
	for page := 0; page < maxPages; page++ {
		b.mu.Lock()
		b.pollSeq++
		id := fmt.Sprintf("vornik-bridge-poll-%d", b.pollSeq)
		b.mu.Unlock()
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list"}
		if cursor != "" {
			req["params"] = map[string]any{"cursor": cursor}
		}
		body, _ := json.Marshal(req)
		idJSON, _ := json.Marshal(id)
		var tools []any
		next, got := "", false
		_, err := post(ctx, hc, b.cfg, session, version, body, func(m []byte) {
			if sameID(m, idJSON) {
				tools, next, got = toolsPage(m)
			}
		})
		if err != nil {
			return nil, err
		}
		if !got {
			return nil, errors.New("the reply carried no tool list")
		}
		all = append(all, tools...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("the tool list ran past %d pages", maxPages)
}
