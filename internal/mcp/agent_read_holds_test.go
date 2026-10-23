package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. The unit tests of the held set and the re-list
// (agent-administered design §6.5, amendment #481), beside the integration
// tests in internal/service (agentadmin_read_sticky_test.go).

// memStore is a ReadSuspensionStore in memory; fail makes writes fail,
// loadErr makes loads fail, moot makes writes moot.
type memStore struct {
	mu      sync.Mutex
	held    map[string]map[string][]string
	writes  int
	fail    bool
	moot    bool
	noRow   bool
	loadErr error
	// beforeWrite runs inside SuspendReadTools before the write lands,
	// without the store's lock: the seam for a load racing a write.
	beforeWrite func()
}

func (s *memStore) LoadReadSuspensions(context.Context) (map[string]map[string][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := map[string]map[string][]string{}
	for p, bs := range s.held {
		out[p] = map[string][]string{}
		for srv, tools := range bs {
			out[p][srv] = append([]string(nil), tools...)
		}
	}
	return out, nil
}

func (s *memStore) SuspendReadTools(_ context.Context, p, srv string, tools []string, _ time.Time) error {
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	switch {
	case s.fail:
		return errors.New("database refused the write")
	case s.moot:
		return ErrSuspensionMoot
	case s.noRow:
		return ErrSuspensionNoApproval
	}
	if s.held == nil {
		s.held = map[string]map[string][]string{}
	}
	if s.held[p] == nil {
		s.held[p] = map[string][]string{}
	}
	s.held[p][srv] = append(s.held[p][srv], tools...)
	return nil
}

func (s *memStore) clear() {
	s.mu.Lock()
	s.held = nil
	s.mu.Unlock()
}

// A held tool is refused and hidden even when the current listing reads;
// the verdicts report it as Held, not Suspended.
func TestReadHolds481_HeldToolIsRefusedAndHidden(t *testing.T) {
	m := rejudgeManager(t, "hermes--comms", "mail", []string{"search_messages", "get_thread"},
		Tool{Name: "search_messages"}, Tool{Name: "get_thread"})
	store := &memStore{held: map[string]map[string][]string{"hermes--comms": {"mail": {"search_messages"}}}}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__get_thread", "{}"); !errors.Is(err, ErrReadSuspensionsUnread) {
		t.Fatalf("an unread store does not refuse: %v", err)
	}
	if len(m.Tools("hermes--comms")) != 0 {
		t.Fatal("an unread store still offers agent read tools")
	}
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a held tool is not refused: %v", err)
	}
	if _, _, err := m.CallToolOnce(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("CallToolOnce: %v", err)
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__get_thread", "{}"); errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("an unheld read is refused: %v", err)
	}
	var names []string
	for _, tl := range m.Tools("hermes--comms") {
		names = append(names, tl.Function.Name)
	}
	if strings.Join(names, ",") != "mcp__mail__get_thread" {
		t.Fatalf("catalogue %v", names)
	}
	connected, _ := m.AgentReadVerdicts()
	if len(connected) != 1 || strings.Join(connected[0].Held, ",") != "search_messages" || len(connected[0].Suspended) != 0 {
		t.Fatalf("verdicts %+v", connected)
	}
	if got := strings.Join(m.HeldReads("hermes--comms", "mail"), ","); got != "search_messages" {
		t.Fatalf("held reads %q", got)
	}
	// The person re-approved (the row was cleared): the next load drops it.
	store.clear()
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a cleared hold still refuses: %v", err)
	}
}

// A failed load keeps the set it had; a store never read stays unread.
func TestReadHolds481_FailedLoadKeepsTheHeldSet(t *testing.T) {
	m := rejudgeManager(t, "hermes--comms", "mail", []string{"search_messages"}, Tool{Name: "search_messages"})
	store := &memStore{held: map[string]map[string][]string{"hermes--comms": {"mail": {"search_messages"}}}}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	store.clear()
	store.loadErr = errors.New("database down")
	if err := m.LoadReadSuspensions(ctx); err == nil {
		t.Fatal("a failed load reported success")
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a failed load dropped the held set: %v", err)
	}
	st := m.ReadHoldState()
	if !st.Wired || !st.Loaded || st.LoadErr == nil {
		t.Fatalf("state %+v", st)
	}
}

// A build whose listing suspends a tool holds it in memory and stores it;
// a failed write is held and retried at the next pass; a moot retry is
// dropped and the store read again.
func TestReadHolds481_BuildRecordsAndRetries(t *testing.T) {
	m := NewManager(zerolog.Nop())
	store := &memStore{fail: true}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	c := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: destructive}})
	m.judgeListing("hermes--comms", c)
	// The server drops the mark: a clean client is installed.
	clean := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: &ToolAnnotations{ReadOnlyHint: boolPtr(true)}}})
	m.mu.Lock()
	m.clients["hermes--comms"] = map[string]*Client{"mail": clean}
	m.mu.Unlock()
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__get_purge", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a failed write lost the hold: %v", err)
	}
	// A load keeps what the store has not taken.
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__get_purge", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a load dropped an unwritten hold: %v", err)
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	m.retrySuspensionWrites(ctx)
	if got := store.held["hermes--comms"]["mail"]; strings.Join(got, ",") != "get_purge" {
		t.Fatalf("the retry did not store it: %v", got)
	}
	m.retrySuspensionWrites(ctx)
	if store.writes != 2 {
		t.Fatalf("a stored retry was written again (%d writes)", store.writes)
	}

	// Moot: the approval was re-made after that listing. The retry is
	// dropped and memory re-read, so the hold ends.
	m2 := NewManager(zerolog.Nop())
	s2 := &memStore{fail: true}
	m2.SetReadSuspensions(s2)
	_ = m2.LoadReadSuspensions(ctx)
	m2.judgeListing("hermes--comms", c)
	m2.mu.Lock()
	m2.clients["hermes--comms"] = map[string]*Client{"mail": clean}
	m2.mu.Unlock()
	s2.mu.Lock()
	s2.fail, s2.moot = false, true
	s2.mu.Unlock()
	m2.retrySuspensionWrites(ctx)
	if _, err := m2.Execute(ctx, "hermes--comms", "mcp__mail__get_purge", "{}"); errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a moot retry kept holding: %v", err)
	}
}

// The re-list re-dials an agent read entry from its own config and installs
// the fresh client; it skips a -write entry, an operator project and a
// client younger than the interval.
func TestReadHolds481_RelistReplacesOnlyDueAgentReadEntries(t *testing.T) {
	var dialled []string
	var mu sync.Mutex
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		mu.Lock()
		dialled = append(dialled, cfg.Name)
		mu.Unlock()
		c := newFakeClient(cfg, []Tool{{Name: "search_messages", Annotations: destructive}})
		c.listedAt = time.Now()
		return c, nil
	})
	m := NewManager(zerolog.Nop())
	old := func(name string) *Client {
		c := newFakeClient(ServerConfig{Name: name, AllowedTools: []string{"search_messages"}}, []Tool{{Name: "search_messages"}})
		c.listedAt = time.Now().Add(-time.Hour)
		return c
	}
	young := old("young")
	young.listedAt = time.Now()
	read := old("mail")
	m.clients["hermes--comms"] = map[string]*Client{"mail": read, "mail-write": old("mail-write"), "young": young}
	m.clients["operator"] = map[string]*Client{"mail": old("mail")}
	m.RelistAgentReads(context.Background(), 15*time.Minute)
	if strings.Join(dialled, ",") != "mail" {
		t.Fatalf("dialled %v, want only the due agent read entry", dialled)
	}
	if m.clients["hermes--comms"]["mail"] == read {
		t.Fatal("the fresh client was not installed")
	}
	if _, err := m.Execute(context.Background(), "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("the re-list's verdict is not applied: %v", err)
	}
}

// A reload that installs another client between the re-list's dial and its
// install wins: the re-list's client is discarded (and still records).
func TestReadHolds481_RelistLosesToAReloadInBetween(t *testing.T) {
	m := NewManager(zerolog.Nop())
	current := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"search_messages"}}, []Tool{{Name: "search_messages"}})
	m.clients["hermes--comms"] = map[string]*Client{"mail": current}
	winner := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"search_messages"}}, []Tool{{Name: "search_messages"}})
	var closed atomic.Int64
	origConnect, origClose := connectFn, closeFn
	t.Cleanup(func() { connectFn, closeFn = origConnect, origClose })
	closeFn = func(*Client) error { closed.Add(1); return nil }
	connectFn = func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		// The reload, deterministically between the dial and the install.
		m.mu.Lock()
		m.clients["hermes--comms"] = map[string]*Client{"mail": winner}
		m.mu.Unlock()
		return newFakeClient(cfg, []Tool{{Name: "search_messages", Annotations: destructive}}), nil
	}
	m.RelistAgentReads(context.Background(), 0)
	if m.clients["hermes--comms"]["mail"] != winner {
		t.Fatal("the re-list overwrote the reload's client")
	}
	// The losing build still recorded what its server said.
	if _, err := m.Execute(context.Background(), "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("the losing build did not hold the tool: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("the discarded client was not closed")
	}
}

// A failed re-list keeps the current client; while the loop runs, a
// listing older than twice the interval is refused, and only then.
func TestReadHolds481_StaleListingRefusedOnlyWhileTheLoopRuns(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("server refuses new sessions")
	})
	m := NewManager(zerolog.Nop())
	c := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"search_messages"}}, []Tool{{Name: "search_messages"}})
	c.listedAt = time.Now().Add(-31 * time.Minute)
	m.clients["hermes--comms"] = map[string]*Client{"mail": c}
	ctx := context.Background()
	m.RelistAgentReads(ctx, 15*time.Minute)
	if m.clients["hermes--comms"]["mail"] != c {
		t.Fatal("a failed re-list dropped the client")
	}
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); errors.Is(err, ErrReadListingStale) {
		t.Fatalf("a listing aged out with no loop running: %v", err)
	}
	m.setRelistInterval(15 * time.Minute)
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadListingStale) {
		t.Fatalf("a listing older than twice the interval still vouches: %v", err)
	}
	c.listedAt = time.Now().Add(-29 * time.Minute)
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); errors.Is(err, ErrReadListingStale) {
		t.Fatalf("a listing younger than twice the interval is refused: %v", err)
	}
}

// A tick reads an unread store at once, and a read store again only once
// the interval has passed.
func TestReadHolds481_TickLoadsUnreadStoreAndThenEveryInterval(t *testing.T) {
	m := NewManager(zerolog.Nop())
	now := time.Now()
	m.now = func() time.Time { return now }
	store := &memStore{loadErr: errors.New("database down")}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	last := now
	m.relistTickOnce(ctx, 15*time.Minute, &last)
	if m.ReadHoldState().Loaded {
		t.Fatal("a failed load counted as read")
	}
	store.mu.Lock()
	store.loadErr = nil
	store.held = map[string]map[string][]string{"hermes--comms": {"mail": {"x"}}}
	store.mu.Unlock()
	m.relistTickOnce(ctx, 15*time.Minute, &last)
	if !m.ReadHoldState().Loaded || !m.holds.holdsLocked("hermes--comms", "mail", "x") {
		t.Fatal("an unread store was not read at the next tick")
	}
	store.clear()
	now = now.Add(time.Minute)
	m.relistTickOnce(ctx, 15*time.Minute, &last)
	if !m.holds.holdsLocked("hermes--comms", "mail", "x") {
		t.Fatal("a read store was read again before the interval")
	}
	now = now.Add(15 * time.Minute)
	m.relistTickOnce(ctx, 15*time.Minute, &last)
	if m.holds.holdsLocked("hermes--comms", "mail", "x") {
		t.Fatal("the store was not read again after the interval")
	}
}

// A load that runs while a build's write is in flight (a reload racing a
// re-list) keeps the hold the write has not landed yet.
func TestReadHolds481_LoadDuringAWriteKeepsTheHold(t *testing.T) {
	m := NewManager(zerolog.Nop())
	store := &memStore{}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	store.beforeWrite = func() { _ = m.LoadReadSuspensions(ctx) } // reads the store before the write lands
	c := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: destructive}})
	m.judgeListing("hermes--comms", c)
	m.holds.mu.RLock()
	held := m.holds.holdsLocked("hermes--comms", "mail", "get_purge")
	pending := len(m.holds.retry)
	m.holds.mu.RUnlock()
	if !held {
		t.Fatal("a load racing the write dropped the hold")
	}
	if pending != 0 {
		t.Fatalf("a stored write stayed in the retry set (%d)", pending)
	}
}

// A write that finds no live approval row yet (the apply's own dial, before
// the grant is written) stays held and queued while the server is
// connected, is stored once the row is live, and is dropped once the
// server is gone (adversarial review F1).
func TestReadHolds481_NoApprovalYetIsRetriedWhileConnected(t *testing.T) {
	m := NewManager(zerolog.Nop())
	store := &memStore{noRow: true}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	_ = m.LoadReadSuspensions(ctx)
	c := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: destructive}})
	m.mu.Lock()
	m.clients["hermes--comms"] = map[string]*Client{"mail": c}
	m.mu.Unlock()
	m.judgeListing("hermes--comms", c)
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	m.holds.mu.RLock()
	held, queued := m.holds.holdsLocked("hermes--comms", "mail", "get_purge"), len(m.holds.retry)
	m.holds.mu.RUnlock()
	if !held || queued != 1 {
		t.Fatalf("a write with no row yet was dropped: held %v queued %d", held, queued)
	}
	store.mu.Lock()
	store.noRow = false
	store.mu.Unlock()
	m.retrySuspensionWrites(ctx)
	if got := store.held["hermes--comms"]["mail"]; strings.Join(got, ",") != "get_purge" {
		t.Fatalf("the retry did not store it once the row was live: %v", got)
	}
	// Gone: a queued write for a server with no client is dropped.
	store.mu.Lock()
	store.noRow = true
	store.mu.Unlock()
	m.judgeListing("hermes--other", newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: destructive}}))
	m.retrySuspensionWrites(ctx)
	m.holds.mu.RLock()
	_, still := m.holds.retry[[2]string{"hermes--other", "mail"}]
	m.holds.mu.RUnlock()
	if still {
		t.Fatal("a queued write for a server with no client was kept")
	}
}

// A load whose read of the store raced a stored write keeps what memory
// held (adversarial review F3): the write lands after the read but before
// the load installs its set.
func TestReadHolds481_LoadRacingAStoredWriteKeepsTheHold(t *testing.T) {
	m := NewManager(zerolog.Nop())
	store := &raceStore{memStore: &memStore{}}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	_ = m.LoadReadSuspensions(ctx)
	c := newFakeClient(ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}, []Tool{{Name: "get_purge", Annotations: destructive}})
	// The load reads the (empty) store, then the write lands, then the
	// load installs its set.
	store.afterRead = func() {
		store.afterRead = nil
		m.judgeListing("hermes--comms", c)
	}
	if err := m.LoadReadSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	m.holds.mu.RLock()
	held := m.holds.holdsLocked("hermes--comms", "mail", "get_purge")
	m.holds.mu.RUnlock()
	if !held {
		t.Fatal("a load that raced a stored write dropped the hold")
	}
}

// raceStore runs afterRead after a load has read the store and before it
// returns: the seam for a write landing inside a load.
type raceStore struct {
	*memStore
	afterRead func()
}

func (r *raceStore) LoadReadSuspensions(ctx context.Context) (map[string]map[string][]string, error) {
	out, err := r.memStore.LoadReadSuspensions(ctx)
	if f := r.afterRead; f != nil {
		f()
	}
	return out, err
}

// A queued suspension of a server that is disconnected (pending a dial) is
// kept and the tool stays held; after the reconnect the tool is still
// refused and the suspension is stored (security review: fail closed).
func TestReadHolds481_DisconnectedServerKeepsItsQueuedSuspension(t *testing.T) {
	m := NewManager(zerolog.Nop())
	store := &memStore{fail: true}
	m.SetReadSuspensions(store)
	ctx := context.Background()
	_ = m.LoadReadSuspensions(ctx)
	cfg := ServerConfig{Name: "mail", AllowedTools: []string{"get_purge"}}
	m.judgeListing("hermes--comms", newFakeClient(cfg, []Tool{{Name: "get_purge", Annotations: destructive}}))
	// Disconnected: configured, pending a dial, no client.
	m.mu.Lock()
	m.pending["hermes--comms"] = map[string]*pendingServer{"mail": newPendingServer(cfg, errors.New("down"), time.Now())}
	m.mu.Unlock()
	store.mu.Lock()
	store.fail, store.noRow = false, true // the write does not land yet
	store.mu.Unlock()
	m.retrySuspensionWrites(ctx)
	_ = m.LoadReadSuspensions(ctx)
	m.holds.mu.RLock()
	queued := len(m.holds.retry)
	m.holds.mu.RUnlock()
	if queued != 1 {
		t.Fatalf("a disconnected server's queued suspension was dropped (%d queued)", queued)
	}
	// Reconnect with a listing that no longer marks the tool.
	clean := newFakeClient(cfg, []Tool{{Name: "get_purge", Annotations: &ToolAnnotations{ReadOnlyHint: boolPtr(true)}}})
	m.mu.Lock()
	delete(m.pending, "hermes--comms")
	m.clients["hermes--comms"] = map[string]*Client{"mail": clean}
	m.mu.Unlock()
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__get_purge", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("the tool runs as a read after the reconnect: %v", err)
	}
	store.mu.Lock()
	store.noRow = false
	store.mu.Unlock()
	m.retrySuspensionWrites(ctx)
	if got := store.held["hermes--comms"]["mail"]; strings.Join(got, ",") != "get_purge" {
		t.Fatalf("the suspension was not stored after the reconnect: %v", got)
	}
}
