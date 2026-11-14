package ui

// Web chat turns run in the background (web-chat-design.md §2.2 to §2.5,
// GREEN review-20261114-c0e8). A send returns at once with a turn id; the
// page polls the turn for its status line and, at the end, the rendered
// exchange. Before this a send held the HTTP request for the whole turn
// (about three minutes and nine model calls measured on 2026-11-14), the page
// showed nothing meanwhile, and a dropped connection cancelled the turn.
// Review ids below (review-YYYYMMDD-xxxx) are the design reviews recorded in
// https://docs.vornik.io

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/webchat"
)

// chatTurnsMax caps the turns running daemon-wide (a var so a test can lower
// it); chatTurnKeep is how long a finished turn stays pollable.
var chatTurnsMax = 64

const chatTurnKeep = 10 * time.Minute

// A streamed partial keeps its first chatPartialHead bytes and a rolling
// last chatPartialTail (web-chat-design.md §3.2), so the receiver's closing
// notice always shows however long the reply.
const (
	chatPartialHead = 256 << 10
	chatPartialTail = 4 << 10
)

type chatTurnState string

const (
	chatTurnRunning   chatTurnState = "running"
	chatTurnDone      chatTurnState = "done"
	chatTurnFailed    chatTurnState = "failed"
	chatTurnCancelled chatTurnState = "cancelled"
)

// chatTurn is one background turn. Fields after the identity are guarded by
// the registry's mutex.
type chatTurn struct {
	id, historyKey, projectID, prompt string

	state      chatTurnState
	status     string
	html       template.HTML
	errMsg     string
	history    []chat.Message // the rendered history, for the no-JS page
	marks      map[int]string
	header     http.Header // the Art 50(2) mark headers of the reply
	cancel     context.CancelFunc
	stopAsked  bool
	finishedAt time.Time
	done       chan struct{}

	// The streamed partial reply (§3.2): the head, then a rolling tail once
	// the head is full; partialLen counts every byte received. The rendered
	// HTML is cached by that length.
	partialHead, partialTail string
	partialPending           string // an unfinished UTF-8 sequence, held until its next delta
	partialTrunc             bool
	partialLen               int
	partialHTML              template.HTML
	partialHTMLLen           int
}

// chatTurnView is a copy of a turn's state, safe to read without the lock.
type chatTurnView struct {
	ID, Prompt, Status, Error string
	State                     chatTurnState
	HTML                      template.HTML
	History                   []chat.Message
	Marks                     map[int]string
	Header                    http.Header
}

type chatTurnRegistry struct {
	mu      sync.Mutex
	byID    map[string]*chatTurn
	running map[string]*chatTurn // history key → its running turn
	now     func() time.Time
}

var (
	errChatTurnRunning = errors.New("a turn is already running for this chat")
	errChatTurnsBusy   = errors.New("too many chat turns are running")
)

func newChatTurnRegistry() *chatTurnRegistry {
	return &chatTurnRegistry{byID: map[string]*chatTurn{}, running: map[string]*chatTurn{}, now: time.Now}
}

func (g *chatTurnRegistry) sweepLocked() {
	for id, t := range g.byID {
		if t.state != chatTurnRunning && g.now().Sub(t.finishedAt) > chatTurnKeep {
			delete(g.byID, id)
		}
	}
}

// start registers a running turn. The check and the insert share one lock,
// so two concurrent sends for one history give one turn and one refusal.
func (g *chatTurnRegistry) start(historyKey, projectID, prompt string, cancel context.CancelFunc) (*chatTurn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	if _, ok := g.running[historyKey]; ok {
		return nil, errChatTurnRunning
	}
	if len(g.running) >= chatTurnsMax {
		return nil, errChatTurnsBusy
	}
	var raw [16]byte
	if _, err := chatRandRead(raw[:]); err != nil {
		return nil, fmt.Errorf("turn id: %w", err)
	}
	t := &chatTurn{
		id: hex.EncodeToString(raw[:]), historyKey: historyKey, projectID: projectID, prompt: prompt,
		state: chatTurnRunning, cancel: cancel, done: make(chan struct{}),
	}
	g.byID[t.id] = t
	g.running[historyKey] = t
	return t, nil
}

func (t *chatTurn) viewLocked() chatTurnView {
	return chatTurnView{
		ID: t.id, Prompt: t.prompt, Status: t.status, Error: t.errMsg, State: t.state,
		HTML: t.html, History: t.history, Marks: t.marks, Header: t.header,
	}
}

// lookup returns the turn when it belongs to this history key and project;
// a turn of another browser, view or project is "not found" (no oracle).
func (g *chatTurnRegistry) lookup(id, historyKey, projectID string) (chatTurnView, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked()
	t, ok := g.byID[id]
	if !ok || t.historyKey != historyKey || t.projectID != projectID {
		return chatTurnView{}, false
	}
	return t.viewLocked(), true
}

// runningFor returns the running turn of a history key in a project.
func (g *chatTurnRegistry) runningFor(historyKey, projectID string) (chatTurnView, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.running[historyKey]
	if !ok || t.projectID != projectID {
		return chatTurnView{}, false
	}
	return t.viewLocked(), true
}

// setStatus updates the running turn of a history key. A line for a key with
// no running turn (it arrived after its turn ended) is dropped.
func (g *chatTurnRegistry) setStatus(historyKey, line string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.running[historyKey]; ok {
		t.status = line
	}
}

// stop asks a running turn to stop. It reports whether the turn exists for
// this requester; stopping a finished turn changes nothing.
func (g *chatTurnRegistry) stop(id, historyKey, projectID string) bool {
	g.mu.Lock()
	t, ok := g.byID[id]
	if !ok || t.historyKey != historyKey || t.projectID != projectID {
		g.mu.Unlock()
		return false
	}
	var cancel context.CancelFunc
	if t.state == chatTurnRunning {
		t.stopAsked = true
		cancel = t.cancel
	}
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// appendPartial adds a streamed delta to a running turn. It reports false
// for a turn that already ended: the delta is dropped.
func (g *chatTurnRegistry) appendPartial(t *chatTurn, delta string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.state != chatTurnRunning {
		return false
	}
	t.partialLen += len(delta)
	// A multibyte character can be split across two deltas: hold back an
	// unfinished sequence until the rest arrives (review-20261114-bb1a F2).
	delta = t.partialPending + delta
	t.partialPending = ""
	if cut := completeRunes(delta); cut < len(delta) {
		t.partialPending, delta = delta[cut:], delta[:cut]
	}
	if !t.partialTrunc {
		room := chatPartialHead - len(t.partialHead)
		if len(delta) <= room {
			t.partialHead += delta
			return true
		}
		cut := room
		for cut > 0 && !utf8.RuneStart(delta[cut]) {
			cut--
		}
		t.partialHead += delta[:cut]
		delta = delta[cut:]
		t.partialTrunc = true
	}
	t.partialTail = lastBytesOnRune(t.partialTail+delta, chatPartialTail)
	return true
}

// completeRunes is the length of s's prefix that ends on a complete UTF-8
// sequence: at most the last three bytes are an unfinished one.
func completeRunes(s string) int {
	for i := len(s) - 1; i >= 0 && i >= len(s)-3; i-- {
		if utf8.RuneStart(s[i]) {
			if utf8.FullRuneInString(s[i:]) {
				return len(s)
			}
			return i
		}
	}
	return len(s)
}

// lastBytesOnRune keeps at most the last n bytes of s, starting on a rune.
func lastBytesOnRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func (t *chatTurn) partialTextLocked() string {
	if !t.partialTrunc {
		return t.partialHead
	}
	return t.partialHead + "\n\n…\n\n" + t.partialTail
}

// partialOf returns a turn's partial text and the bytes received so far.
func (g *chatTurnRegistry) partialOf(t *chatTurn) (string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return t.partialTextLocked(), t.partialLen
}

// renderedPartial returns a turn's partial rendered by render, from the
// cache when no delta arrived since. It renders outside the lock.
func (g *chatTurnRegistry) renderedPartial(id string, render func(string) template.HTML) template.HTML {
	g.mu.Lock()
	t, ok := g.byID[id]
	if !ok || t.partialLen == 0 {
		g.mu.Unlock()
		return ""
	}
	if t.partialHTMLLen == t.partialLen {
		h := t.partialHTML
		g.mu.Unlock()
		return h
	}
	text, n := t.partialTextLocked(), t.partialLen
	g.mu.Unlock()
	h := render(text)
	g.mu.Lock()
	if t.partialLen == n {
		t.partialHTML, t.partialHTMLLen = h, n
	}
	g.mu.Unlock()
	return h
}

func (g *chatTurnRegistry) stopAsked(t *chatTurn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return t.stopAsked
}

// finish ends a running turn once; later calls are no-ops.
func (g *chatTurnRegistry) finish(t *chatTurn, state chatTurnState, fill func(*chatTurn)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.state != chatTurnRunning {
		return
	}
	t.state = state
	if fill != nil {
		fill(t)
	}
	t.finishedAt = g.now()
	if g.running[t.historyKey] == t {
		delete(g.running, t.historyKey)
	}
	close(t.done)
}

// WithLongWorkTracker wires the API server's in-flight counter for work that
// outlives its request (api.Server.TrackLongWork), so the deploy idle gate
// waits for a running chat turn and a draining daemon refuses a new one.
func WithLongWorkTracker(f func() (done func(), ok bool)) ServerOption {
	return func(s *Server) { s.longWork = f }
}

func (s *Server) chatTurnRegistry() *chatTurnRegistry {
	s.chatTurnsOnce.Do(func() { s.chatTurns = newChatTurnRegistry() })
	return s.chatTurns
}

func (s *Server) chatTurnLimit() time.Duration {
	if s.chatTurnTimeout > 0 {
		return s.chatTurnTimeout
	}
	return chatDispatchTimeout
}

// SetWebChatTurnStatus receives the dispatcher's status line for a web chat
// turn (the service's turn status reporter calls it; web-chat-design.md
// §2.3). The page shows its own spinner, so the line's hourglass is dropped.
func (s *Server) SetWebChatTurnStatus(historyKey, line string) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "⏳"))
	s.chatTurnRegistry().setStatus(historyKey, line)
}

// chatTurnInput is everything a turn needs from its request, read before the
// handler returns: nothing reads the request afterwards (review-20261114-6e65 F1).
type chatTurnInput struct {
	projectID, prompt, sessionID, historyKey string
	operatorView                             bool
	principal                                string
	// keepHistory: the no-JS send renders the turn's history, so the turn
	// keeps it; a script's turn does not (the poll returns only the HTML),
	// so a finished turn does not hold a transcript for ten minutes.
	keepHistory bool
	// stream: the script's turn streams its reply for the page to poll
	// (§3.2); the no-JS turn does not.
	stream bool
}

// prepareChatTurn runs the checks a send has always run (after the #491
// refusal, which each handler makes first). On a problem it returns the page
// data to render, the status and a message; otherwise the turn's input.
func (s *Server) prepareChatTurn(w http.ResponseWriter, r *http.Request, projectID string) (chatTurnInput, ChatPageData, int, string) {
	if err := r.ParseForm(); err != nil {
		return chatTurnInput{}, ChatPageData{}, http.StatusBadRequest, "Failed to parse form: " + err.Error()
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	data := s.chatPageData(r, projectID)
	s.ensureChatCookie(w, r, &data)
	switch {
	case data.Disabled:
		return chatTurnInput{}, data, http.StatusServiceUnavailable, data.Error
	case prompt == "":
		return chatTurnInput{}, data, http.StatusBadRequest, "Prompt is empty — type something to send."
	case data.ProjectID == "" || (data.ProjectName == "" && s.projectReg != nil && s.projectReg.GetProject(projectID) == nil):
		return chatTurnInput{}, data, http.StatusNotFound, "Project not found in the registry."
	}
	operatorView := s.chatOperatorView(r)
	in := chatTurnInput{
		projectID: projectID, prompt: prompt, sessionID: data.SessionID,
		historyKey: chatHistoryKey(data.SessionID, operatorView), operatorView: operatorView,
	}
	// #499: the web chat's tasks carry the request's principal (the
	// presented key first, then the session user).
	if a := s.uiSessionActor(r); !a.IsZero() {
		in.principal = a.String()
	}
	return in, data, 0, ""
}

// beginChatTurn starts a turn in the background. On a refusal it returns the
// status, an error code and a message.
func (s *Server) beginChatTurn(r *http.Request, in chatTurnInput) (*chatTurn, int, string, string) {
	release := func() {}
	if s.longWork != nil {
		done, ok := s.longWork()
		if !ok {
			return nil, http.StatusServiceUnavailable, "DRAINING", "Vornik is restarting. Send it again in a moment."
		}
		release = done
	}
	// WithoutCancel keeps the request's values (principal, session role,
	// project scope, real IP) and drops its cancellation: leaving or
	// reloading the page no longer kills the turn. The cap stays.
	// The cause sits outermost, so a Stop reaches the receiver as
	// dispatcher.ErrStoppedByUser and it ends a streamed reply with its own
	// "[Stopped.]" (web-chat-design.md §3.2).
	base, cancelBase := context.WithTimeout(context.WithoutCancel(r.Context()), s.chatTurnLimit())
	ctx, cancelCause := context.WithCancelCause(base)
	cancel := func() { cancelCause(nil); cancelBase() }
	stop := func() { cancelCause(dispatcher.ErrStoppedByUser) }
	t, err := s.chatTurnRegistry().start(in.historyKey, in.projectID, in.prompt, stop)
	if err != nil {
		cancel()
		release()
		switch {
		case errors.Is(err, errChatTurnRunning):
			return nil, http.StatusConflict, "TURN_RUNNING", "A reply is still being written. Wait for it, or stop it."
		case errors.Is(err, errChatTurnsBusy):
			return nil, http.StatusServiceUnavailable, "TURNS_BUSY", "Too many replies are being written right now. Try again in a minute."
		default:
			return nil, http.StatusInternalServerError, "TURN_FAILED", "The reply could not be started."
		}
	}
	go s.runChatTurn(ctx, cancel, release, t, in)
	return t, http.StatusAccepted, "", ""
}

// runChatTurn is the turn: the receiver as the synchronous handler ran it,
// then the rendered exchange. It recovers a panic (no HTTP server recovers
// this goroutine) and always releases the gate count.
func (s *Server) runChatTurn(ctx context.Context, cancel func(), release func(), t *chatTurn, in chatTurnInput) {
	reg := s.chatTurnRegistry()
	defer release()
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			s.logger.Error().Interface("panic", p).Str("project", in.projectID).Msg("web chat: turn panicked")
			reg.finish(t, chatTurnFailed, func(t *chatTurn) { t.errMsg = "Something went wrong." })
		}
	}()

	store := s.chatStoreFor(in.projectID)
	speaker := conversation.Speaker{ID: "web-chat:" + in.sessionID, DisplayName: "Web operator"}
	channel := webchat.New(in.projectID, speaker)
	var conv conversation.Channel = channel
	if in.stream {
		// The script's turn streams its reply into the registry, which the
		// page polls (§3.2); the no-JS turn stays one-shot.
		sc := webchat.NewStreaming(in.projectID, speaker, func(delta string) {
			if !reg.appendPartial(t, delta) {
				s.logger.Debug().Str("project", in.projectID).Msg("web chat: a streamed delta arrived after its turn ended; dropped")
			}
		})
		channel, conv = sc.Channel, sc
	}
	receiver := s.chatReceiver(conv, store, in)
	inbound := conversation.ChannelMessage{
		Source: channel.Name(), SessionID: in.historyKey, SpeakerID: in.sessionID,
		Text: in.prompt, Timestamp: time.Now(),
	}
	if err := receiver.Receive(ctx, inbound); err != nil {
		state, msg := chatTurnFailed, "Chat dispatch failed: "+err.Error()
		switch {
		case reg.stopAsked(t):
			state, msg = chatTurnCancelled, ""
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			msg = fmt.Sprintf("The assistant did not answer in %s.", s.chatTurnLimit().Round(time.Second))
		}
		// What streamed was delivered: it stays on the page, marked
		// (Art 50(2), §3.2), though it is not stored.
		partialHTML, header := s.markedPartial(ctx, reg, t, in.projectID)
		reg.finish(t, state, func(t *chatTurn) {
			t.errMsg, t.html, t.header = msg, partialHTML, header
		})
		return
	}
	stored := store.History(in.historyKey)
	history := mergeDisclosureIntoHistory(stored, channel, s.aiDisclosure)
	header := http.Header{}
	// Every text this turn delivered, from the stored history: the interim
	// sentences (streaming delivers them) and the reply (§3.2).
	marks := s.markChatReplies(ctx, header, turnTexts(history), history)
	exchanges := buildChatExchanges(history, marks)
	// The new exchange, and the disclosure notice when it was served this turn.
	var fresh []chatExchange
	if len(history) > len(stored) && len(exchanges) > 1 && exchanges[0].User == "" {
		fresh = append(fresh, exchanges[0])
	}
	if len(exchanges) > 0 {
		fresh = append(fresh, exchanges[len(exchanges)-1])
	}
	html := s.renderChatExchanges(fresh, in.projectID)
	if html == "" {
		// The reply is stored, but the page cannot show it: say so, rather
		// than a "done" with nothing in it (review-20261114-3435 F3).
		reg.finish(t, chatTurnFailed, func(t *chatTurn) { t.errMsg = "Something went wrong showing the reply. Reload the page to see it." })
		return
	}
	reg.finish(t, chatTurnDone, func(t *chatTurn) {
		t.html, t.marks, t.header = html, marks, header
		if in.keepHistory {
			t.history = history
		}
	})
}

// turnTexts is every assistant text after the history's last user message:
// what this turn delivered.
func turnTexts(history []chat.Message) []string {
	last := -1
	for i, m := range history {
		if m.Role == "user" {
			last = i
		}
	}
	var out []string
	for _, m := range history[last+1:] {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			out = append(out, m.Content)
		}
	}
	return out
}

// markedPartial renders a turn's streamed partial with its Art 50(2) mark,
// for a turn that stopped or failed after delivering text. Empty when
// nothing streamed.
func (s *Server) markedPartial(ctx context.Context, reg *chatTurnRegistry, t *chatTurn, projectID string) (template.HTML, http.Header) {
	text, n := reg.partialOf(t)
	if n == 0 {
		return "", nil
	}
	header := http.Header{}
	mark := ""
	if s.aiMarker != nil {
		// The turn's context has ended (stopped, timed out or failed): mark
		// on a fresh one, so the ledger row is written (review-20261114-bb1a F1).
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		compact, _, err := s.aiMarker.New(markCtx, []byte(text), "web_chat", "")
		if compact == "" {
			s.logger.Warn().Err(err).Str("project", projectID).Msg("web chat: AI output mark not created; partial reply shown unmarked")
		} else {
			if err != nil {
				s.logger.Warn().Err(err).Str("project", projectID).Msg("web chat: partial reply marked, but its ledger row was not recorded")
			}
			mark = compact
			header.Set("AI-Disclosure", "ai-generated")
			header.Set("X-Vornik-AI-Mark", compact)
		}
	}
	return s.renderChatPartial(text, mark), header
}

// renderChatPartial renders streamed text in the reply's markdown container.
func (s *Server) renderChatPartial(text, mark string) template.HTML {
	var b bytes.Buffer
	data := struct {
		HTML template.HTML
		Mark string
	}{renderChatMarkdown(text), mark}
	if err := s.templates.ExecuteTemplate(&b, "chatPartial", data); err != nil {
		s.logger.Error().Err(err).Msg("web chat: render partial")
		return ""
	}
	return template.HTML(b.String()) //nolint:gosec // G203: html/template output; the markdown is goldmark with raw HTML omitted
}

// chatReceiver builds the receiver a web chat turn runs through. Its
// callbacks return the turn's snapshots, never read the request.
func (s *Server) chatReceiver(channel conversation.Channel, store *webchat.SessionStore, in chatTurnInput) *dispatcher.ChannelReceiver {
	return &dispatcher.ChannelReceiver{
		Channel:                  channel,
		Agent:                    s.chatDispatcher,
		Sessions:                 store,
		Disclosure:               s.aiDisclosure,
		DisclosureMetrics:        s.disclosureMetrics,
		Media:                    s.mediaSight,
		MemoryWriteConfirmations: s.chatMemoryConfirmations,
		OperatorView: func(context.Context, conversation.ChannelMessage) bool {
			return in.operatorView
		},
		TaskPrincipal: func(context.Context, conversation.ChannelMessage) string {
			return in.principal
		},
		ResultPostprocessor: func(result dispatcher.Result) string {
			// For the web chat turn, produced files belong to a separate
			// task surface (the dispatcher submits work asynchronously);
			// the postprocessor is a passthrough later code can extend.
			return result.Text
		},
	}
}

// chatExchangesData backs the chatExchanges partial.
type chatExchangesData struct {
	Exchanges []chatExchange
	ProjectID string
}

func (s *Server) renderChatExchanges(ex []chatExchange, projectID string) template.HTML {
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, "chatExchanges", chatExchangesData{Exchanges: ex, ProjectID: projectID}); err != nil {
		s.logger.Error().Err(err).Msg("web chat: render exchange")
		return ""
	}
	return template.HTML(b.String()) //nolint:gosec // G203: html/template output, escaped there
}

// writeChatJSON writes a small JSON body with no-store caching.
func writeChatJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeChatJSONError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	writeChatJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// chatRequestKey is the requester's history key, or "" without a cookie.
func (s *Server) chatRequestKey(r *http.Request) string {
	c, err := r.Cookie(chatSessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	return chatHistoryKey(c.Value, s.chatOperatorView(r))
}

// ChatTurnStart handles POST /ui/projects/<id>/chat/turns: start a turn and
// answer 202 {"turn": id} at once.
func (s *Server) ChatTurnStart(w http.ResponseWriter, r *http.Request, projectID string) {
	if api.ErrorTextAccessFor(r, s.operatorCapability.IsAdminKey) == api.ErrorTextDenied {
		api.RespondProjectScopeRequired(w)
		return
	}
	in, data, status, msg := s.prepareChatTurn(w, r, projectID)
	in.stream = true
	if status != 0 {
		code := "INVALID"
		switch {
		case data.Disabled:
			code = "DISABLED"
		case status == http.StatusNotFound:
			code = "PROJECT_NOT_FOUND"
		}
		writeChatJSONError(w, status, code, msg)
		return
	}
	t, status, code, msg := s.beginChatTurn(r, in)
	if t == nil {
		writeChatJSONError(w, status, code, msg)
		return
	}
	writeChatJSON(w, http.StatusAccepted, map[string]string{"turn": t.id})
}

// ChatTurnPoll handles GET /ui/projects/<id>/chat/turns/<turn>.
func (s *Server) ChatTurnPoll(w http.ResponseWriter, r *http.Request, projectID, turnID string) {
	if api.ErrorTextAccessFor(r, s.operatorCapability.IsAdminKey) == api.ErrorTextDenied {
		api.RespondProjectScopeRequired(w)
		return
	}
	v, ok := s.chatTurnRegistry().lookup(turnID, s.chatRequestKey(r), projectID)
	if !ok {
		writeChatJSONError(w, http.StatusNotFound, "TURN_NOT_FOUND", "This reply was lost, probably in a restart.")
		return
	}
	body := map[string]any{"state": v.State}
	switch v.State {
	case chatTurnRunning:
		body["status"] = v.Status
		if h := s.chatTurnRegistry().renderedPartial(v.ID, func(text string) template.HTML { return s.renderChatPartial(text, "") }); h != "" {
			body["html"] = h
		}
	case chatTurnDone:
		body["html"] = v.HTML
		for k, vals := range v.Header {
			for _, val := range vals {
				w.Header().Add(k, val)
			}
		}
	case chatTurnFailed, chatTurnCancelled:
		if v.Error != "" {
			body["error"] = v.Error
		}
		if v.HTML != "" {
			body["html"] = v.HTML // the delivered partial, marked
			for k, vals := range v.Header {
				for _, val := range vals {
					w.Header().Add(k, val)
				}
			}
		}
	}
	writeChatJSON(w, http.StatusOK, body)
}

// ChatTurnCancel handles POST /ui/projects/<id>/chat/turns/<turn>/cancel.
func (s *Server) ChatTurnCancel(w http.ResponseWriter, r *http.Request, projectID, turnID string) {
	if api.ErrorTextAccessFor(r, s.operatorCapability.IsAdminKey) == api.ErrorTextDenied {
		api.RespondProjectScopeRequired(w)
		return
	}
	if !s.chatTurnRegistry().stop(turnID, s.chatRequestKey(r), projectID) {
		writeChatJSONError(w, http.StatusNotFound, "TURN_NOT_FOUND", "No such reply.")
		return
	}
	writeChatJSON(w, http.StatusOK, map[string]bool{"stopping": true})
}

// ChatReset handles POST /ui/projects/<id>/chat/reset ("New chat"): clear
// this browser's history for the view, refused while a turn runs, because the
// turn's end would write the cleared conversation back (review-20261114-6e65 F3).
func (s *Server) ChatReset(w http.ResponseWriter, r *http.Request, projectID string) {
	if api.ErrorTextAccessFor(r, s.operatorCapability.IsAdminKey) == api.ErrorTextDenied {
		api.RespondProjectScopeRequired(w)
		return
	}
	if s.projectReg != nil && s.projectReg.GetProject(projectID) == nil {
		http.NotFound(w, r)
		return
	}
	key := s.chatRequestKey(r)
	if key != "" {
		// Refused only while this project's chat has a running turn: a turn
		// in another project writes to that project's store, not this one.
		if _, running := s.chatTurnRegistry().runningFor(key, projectID); running {
			http.Error(w, "A reply is still being written; stop it or wait, then start a new chat.", http.StatusConflict)
			return
		}
		s.chatStoreFor(projectID).Reset(key)
	}
	http.Redirect(w, r, "/ui/projects/"+url.PathEscape(projectID)+"/chat", http.StatusSeeOther)
}
