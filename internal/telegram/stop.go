package telegram

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/dispatcher"
)

// /stop (dispatcher turn-context design §8, GitHub #644): end the chat's
// running turn. On 2026-11-04 the operator could only watch a looping turn
// run to the iteration cap, twice.

// stopTasksWindow is how long after a /stop its task list can be cancelled
// with /stop tasks.
const stopTasksWindow = 10 * time.Minute

type runningTurn struct {
	gen      uint64
	cancel   context.CancelCauseFunc
	started  time.Time
	followup bool
	// inbox takes messages sent while a user turn runs (§9, #645); nil for
	// a follow-up delivery.
	inbox *dispatcher.SteerInbox
	// speaker is the user whose message started the turn. Only they may
	// steer it: the turn runs with their project scope, and another
	// member's message must run under that member's own scope.
	speaker int64
}

type stoppedTurn struct {
	taskIDs []string
	at      time.Time
}

// stopRegistry holds each chat's running turn and its last /stop. Its mutex
// is a leaf lock: nothing but map operations run under it, and no other lock
// is acquired while it is held. Callers may already hold the chat's
// receiverLock (beginTurn runs under it).
type stopRegistry struct {
	mu      sync.Mutex
	gen     uint64
	running map[int64]runningTurn
	stopped map[int64]stoppedTurn
	now     func() time.Time // tests set it
}

func (r *stopRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// beginTurn builds the turn's context (the cancel wraps the timeout, so a
// deadline still reads as a timeout and a stop as a cancel) and records it as
// the chat's running turn. done releases both; it removes the entry only if
// it is still this turn's (gen check).
func (b *Bot) beginTurn(chatID int64, followup bool, speaker int64) (context.Context, func()) {
	tctx, tcancel := context.WithTimeout(context.Background(), b.effectiveDispatchTimeout())
	ctx, cancel := context.WithCancelCause(tctx)
	var inbox *dispatcher.SteerInbox
	if !followup {
		inbox = dispatcher.NewSteerInbox()
		ctx = dispatcher.WithSteering(ctx, inbox)
	}
	r := &b.stops
	r.mu.Lock()
	r.gen++
	gen := r.gen
	if r.running == nil {
		r.running = map[int64]runningTurn{}
	}
	r.running[chatID] = runningTurn{gen: gen, cancel: cancel, started: r.clock(), followup: followup, inbox: inbox, speaker: speaker}
	r.mu.Unlock()
	return ctx, func() {
		r.mu.Lock()
		if cur, ok := r.running[chatID]; ok && cur.gen == gen {
			delete(r.running, chatID)
		}
		r.mu.Unlock()
		cancel(nil)
		tcancel()
		// A message that arrived after the turn's last read runs as its own
		// turn, as it would have before steering (§9.2).
		if inbox != nil {
			// After a failure, steered messages that were read but never
			// answered run again as their own turns; after /stop they are
			// dropped with the turn the user ended (review cd48 F2).
			requeue := !errors.Is(context.Cause(ctx), dispatcher.ErrStoppedByUser)
			for _, ref := range inbox.Close(requeue) {
				m, ok := ref.(*Message)
				if !ok {
					b.logger.Error().Int64("chat_id", chatID).Msg("telegram: steered message with an unknown handle dropped")
					continue
				}
				b.startLeftoverTurn(m)
			}
		}
	}
}

// offerSteer gives a plain message to the chat's running user turn. It
// reports whether the turn took it; if not, the caller starts a turn as
// before (§9.2).
func (b *Bot) offerSteer(ctx context.Context, msg *Message) bool {
	// A file or a voice message takes the normal turn path (review cd48 F8).
	if msg == nil || msg.FileID != "" || msg.DownloadedPath != "" || msg.IsVoice {
		return false
	}
	r := &b.stops
	r.mu.Lock()
	turn, ok := r.running[msg.ChatID]
	r.mu.Unlock()
	// Only the turn's own speaker steers it; anyone else's message takes the
	// normal path under their own scope (security review of be0fc3d80).
	if !ok || turn.inbox == nil || turn.speaker != msg.UserID {
		return false
	}
	// The same scrub the turn path applies to its text (handleReceiverTurn).
	text := strings.TrimSpace(b.exposure.Scrub(ctx, "telegram", msg.Text))
	if text == "" {
		return false
	}
	taken := turn.inbox.Offer(text, msg)
	b.logger.Debug().Int64("chat_id", msg.ChatID).Bool("taken", taken).Msg("telegram: message offered to the running turn")
	return taken
}

// startLeftoverTurn runs a message the turn never read as a normal turn.
func (b *Bot) startLeftoverTurn(m *Message) {
	r := b.Receiver()
	if r == nil {
		return
	}
	go b.handleReceiverTurn(r, m)
}

// handleStopCmd answers /stop and /stop tasks.
func (b *Bot) handleStopCmd(ctx context.Context, chatID, userID int64, parts []string) string {
	if len(parts) > 1 && strings.EqualFold(parts[1], "tasks") {
		return b.stopTasks(ctx, chatID, userID)
	}
	r := &b.stops
	r.mu.Lock()
	turn, ok := r.running[chatID]
	r.mu.Unlock()
	if !ok {
		return "Nothing is running in this chat."
	}
	turn.cancel(dispatcher.ErrStoppedByUser)
	ids := b.followupTasksSince(chatID, turn.started)
	if len(ids) > 0 {
		// Only a stop that left tasks is recorded, so "/stop tasks" never
		// claims a stop left tasks when it left none (review 81b7 F5).
		r.mu.Lock()
		if r.stopped == nil {
			r.stopped = map[int64]stoppedTurn{}
		}
		r.stopped[chatID] = stoppedTurn{taskIDs: ids, at: r.clock()}
		r.mu.Unlock()
	}
	b.logger.Info().Int64("chat_id", chatID).Int64("user_id", userID).Strs("tasks", ids).Msg("telegram: /stop ended the running turn")

	var out strings.Builder
	out.WriteString("Stopped.")
	switch len(ids) {
	case 0:
	case 1:
		fmt.Fprintf(&out, " It had started task %s. It keeps running; send /stop tasks to cancel it too.", ids[0])
	default:
		fmt.Fprintf(&out, " It had started %d tasks: %s. They keep running; send /stop tasks to cancel them too.", len(ids), strings.Join(ids, ", "))
	}
	if turn.followup {
		out.WriteString(" It was reporting finished tasks; /tasks lists them.")
	}
	return out.String()
}

// followupTasksSince lists the chat's tasks that registered a completion
// follow-up at or after since, i.e. tasks the turn created and still running.
func (b *Bot) followupTasksSince(chatID int64, since time.Time) []string {
	b.followupMu.Lock()
	defer b.followupMu.Unlock()
	var ids []string
	for id, fu := range b.pendingFollowups {
		if fu.chatID == chatID && !fu.createdAt.Before(since) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// stopTasks cancels the tasks the last /stop listed, as the user's
// confirmation, each through the same scoped path as /cancel.
func (b *Bot) stopTasks(ctx context.Context, chatID, userID int64) string {
	r := &b.stops
	r.mu.Lock()
	st, ok := r.stopped[chatID]
	if ok && r.clock().Sub(st.at) > stopTasksWindow {
		ok = false
	}
	// One-shot: the list is consumed (or, expired, pruned) here. A task whose
	// cancel is refused is reported and left to /cancel.
	delete(r.stopped, chatID)
	r.mu.Unlock()
	if !ok || len(st.taskIDs) == 0 {
		return "No stopped turn left tasks to cancel in the last 10 minutes."
	}
	lines := make([]string, 0, len(st.taskIDs))
	for _, id := range st.taskIDs {
		t, msg := b.loadScopedTask(ctx, userID, []string{"/stop", id}, "")
		if t == nil {
			lines = append(lines, msg)
			continue
		}
		lines = append(lines, b.cancelScopedTask(ctx, t))
	}
	return strings.Join(lines, "\n")
}
