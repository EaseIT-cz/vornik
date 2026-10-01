package companionpush

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Loop settings (design §7a).
const (
	defaultInterval = 30 * time.Second
	passLimit       = 100
	maxPasses       = 10
	attemptsPerPass = 2
)

// Config wires a Pusher.
type Config struct {
	Outbox persistence.CompanionPushOutbox
	// Allowed returns a project's companion_push.allowed_cidrs, read at
	// send time from the live registry. Nil: public addresses only.
	Allowed func(projectID string) []netip.Prefix
	// Interval between passes; default 30s.
	Interval time.Duration
	// OnResult is told each push outcome: kind task|action, result
	// delivered|failed|refused|abandoned. Optional (metrics).
	OnResult func(kind, result string)
	Logger   zerolog.Logger
}

// Pusher is the one push loop per daemon. It reads what is due from the
// outbox, POSTs it, and records delivery with a compare-and-set, so pushes
// survive restarts and cover transitions written by other processes.
type Pusher struct {
	cfg    Config
	client *client
	kick   chan struct{}

	mu       sync.Mutex
	failures map[string]int // "kind|id|state" -> failed passes (in memory)
}

// New builds a pusher.
func New(cfg Config) *Pusher {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Allowed == nil {
		cfg.Allowed = func(string) []netip.Prefix { return nil }
	}
	return &Pusher{cfg: cfg, client: newClient(), kick: make(chan struct{}, 1), failures: map[string]int{}}
}

// Kick asks for a pass now. Never blocks; nil-safe.
func (p *Pusher) Kick() {
	if p == nil {
		return
	}
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// NotifyTaskCompleted makes the pusher an executor completion observer. It
// only kicks: the executor never waits on a push.
func (p *Pusher) NotifyTaskCompleted(context.Context, *persistence.Task, bool, string) {
	p.Kick()
}

// Run passes at start, then on each kick and interval, until ctx ends.
func (p *Pusher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	p.Pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
			p.Pass(ctx)
		case <-ticker.C:
			p.Pass(ctx)
		}
	}
}

// Pass sends every due item once. Without an outbox it does nothing.
func (p *Pusher) Pass(ctx context.Context) {
	if p == nil || p.cfg.Outbox == nil {
		return
	}
	if tasks, err := p.cfg.Outbox.DueTaskPushes(ctx, passLimit); err != nil {
		p.cfg.Logger.Warn().Err(err).Msg("companion push: task outbox read failed")
	} else {
		for _, d := range tasks {
			body, _ := json.Marshal(map[string]string{"task_id": d.TaskID, "state": d.State})
			p.deliver(ctx, "task", d.TaskID, d.ProjectID, d.State, d.URL, d.Token, body, func() (bool, error) {
				return p.cfg.Outbox.MarkTaskPushed(ctx, d.TaskID, d.PushedState, d.State)
			})
		}
	}
	actions, err := p.cfg.Outbox.DueActionPushes(ctx, passLimit)
	if err != nil {
		p.cfg.Logger.Warn().Err(err).Msg("companion push: action outbox read failed")
		return
	}
	for _, d := range actions {
		mark := func() (bool, error) { return p.cfg.Outbox.MarkActionPushed(ctx, d.ActionID, d.PushedState, d.State) }
		if d.URL == "" {
			// The task has no push config: record the state so it is not
			// scanned again. A failed mark only means a re-scan next pass.
			if _, merr := mark(); merr != nil {
				p.cfg.Logger.Warn().Err(merr).Str("action_id", d.ActionID).Msg("companion push: could not mark an unconfigured action; it will be scanned again")
			}
			continue
		}
		body, _ := json.Marshal(map[string]string{"task_id": d.TaskID, "action_id": d.ActionID, "action": d.Action, "state": d.State})
		p.deliver(ctx, "action", d.ActionID, d.ProjectID, d.State, d.URL, d.Token, body, mark)
	}
}

// deliver sends one item and records the outcome.
func (p *Pusher) deliver(ctx context.Context, kind, id, projectID, state, url, token string, body []byte, mark func() (bool, error)) {
	key := kind + "|" + id + "|" + state
	p.mu.Lock()
	failed := p.failures[key]
	p.mu.Unlock()
	if failed >= maxPasses {
		return // abandoned until a restart
	}
	allowed := p.cfg.Allowed(projectID)
	var err error
	for attempt := 0; attempt < attemptsPerPass; attempt++ {
		if err = p.client.post(ctx, url, token, body, allowed); err == nil || errors.Is(err, errRefused) {
			break
		}
	}
	if err == nil {
		if _, merr := mark(); merr != nil {
			p.cfg.Logger.Warn().Err(merr).Str(kind+"_id", id).Msg("companion push: delivered but not recorded; it may be pushed again")
		}
		p.mu.Lock()
		delete(p.failures, key)
		p.mu.Unlock()
		p.result(kind, "delivered")
		return
	}
	result := "failed"
	if errors.Is(err, errRefused) {
		result = "refused"
	}
	p.result(kind, result)
	p.mu.Lock()
	p.failures[key] = failed + 1
	abandoned := failed+1 >= maxPasses
	p.mu.Unlock()
	p.cfg.Logger.Warn().Err(err).Str(kind+"_id", id).Str("state", state).Int("pass", failed+1).
		Msg("companion push: not delivered")
	if abandoned {
		p.result(kind, "abandoned")
		p.cfg.Logger.Warn().Str(kind+"_id", id).Str("state", state).
			Msg("companion push: giving up after 10 passes; the client reads the state with status")
	}
}

func (p *Pusher) result(kind, result string) {
	if p.cfg.OnResult != nil {
		p.cfg.OnResult(kind, result)
	}
}
