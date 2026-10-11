package api

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Reuse of a recent read-only result (broker design §6b, GitHub #119 item 9,
// operator decision F11): delegate(..., max_age_seconds) answers with an
// earlier completed task of the same key, workflow and inputs instead of
// running the broker again. In memory, per daemon, opt-in per call.

const (
	resultReuseMaxEntries    = 1024
	resultReuseMaxAgeSeconds = 900
)

// reuseKey is one cache key (§6b.3): the api key id, the project, the
// workflow and the sha256 of the canonical validated inputs.
type reuseKey struct {
	keyID, project, workflow, inputs string
}

// reuseEntry points at the task that answered; never the answer itself.
type reuseEntry struct {
	taskID, reachHash, definitionHash string
	recordedAt                        time.Time
}

type reuseItem struct {
	key   reuseKey
	entry reuseEntry
}

// resultReuseCache is a bounded LRU; the front of ll is the most recent.
type resultReuseCache struct {
	mu    sync.Mutex
	limit int
	ll    *list.List
	idx   map[reuseKey]*list.Element
	now   func() time.Time
}

func newResultReuseCache(limit int) *resultReuseCache {
	return &resultReuseCache{limit: limit, ll: list.New(), idx: map[reuseKey]*list.Element{}, now: time.Now}
}

func (c *resultReuseCache) get(k reuseKey) (reuseEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[k]
	if !ok {
		return reuseEntry{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*reuseItem).entry, true
}

func (c *resultReuseCache) put(k reuseKey, e reuseEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		el.Value.(*reuseItem).entry = e
		c.ll.MoveToFront(el)
		return
	}
	c.idx[k] = c.ll.PushFront(&reuseItem{key: k, entry: e})
	for c.ll.Len() > c.limit {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.idx, oldest.Value.(*reuseItem).key)
	}
}

func (c *resultReuseCache) drop(k reuseKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		c.ll.Remove(el)
		delete(c.idx, k)
	}
}

func (c *resultReuseCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// resultReuseState is the server's cache, built on first use.
func (s *Server) resultReuseState() *resultReuseCache {
	s.resultReuseOnce.Do(func() {
		if s.resultReuse == nil {
			s.resultReuse = newResultReuseCache(resultReuseMaxEntries)
		}
	})
	return s.resultReuse
}

// WithResultReuseReachHash wires the reach hash a reuse is pinned to: the
// one a standing grant pins (an agent namespace's device-approved hash, an
// operator project's live signature). Without it reuse is off and
// companion-result-reuse is not advertised.
func WithResultReuseReachHash(fn func(ctx context.Context, projectID, workflowID string) (string, error)) ServerOption {
	return func(srv *Server) { srv.reuseReachHash = fn }
}

// parseMaxAge reads delegate's max_age_seconds (§6b.1): absent or 0 is no
// reuse; above 900 is clamped to 900; anything else is refused.
func parseMaxAge(raw json.RawMessage) (int, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return 0, errors.New("INPUT_REJECTED: max_age_seconds must be an integer from 0 to 900")
	}
	if n > resultReuseMaxAgeSeconds {
		n = resultReuseMaxAgeSeconds
	}
	return n, nil
}

// reuseKeyFor builds the cache key from the validated inputs (§6b.3).
func reuseKeyFor(key *persistence.APIKey, projectID, workflowID string, inputs map[string]any) (reuseKey, bool) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return reuseKey{}, false
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return reuseKey{}, false
	}
	sum := sha256.Sum256(canon)
	return reuseKey{keyID: key.ID, project: projectID, workflow: workflowID, inputs: hex.EncodeToString(sum[:])}, true
}

// reuseDefinitionHash covers what the reach signature deliberately ignores
// (§6b.4 check 5): the workflow's own content and the swarm roles its steps
// run, so a changed instruction or role misses.
func reuseDefinitionHash(wf *registry.Workflow, sw *registry.Swarm) (string, error) {
	used := map[string]bool{}
	for _, st := range wf.Steps {
		if st.Role != "" {
			used[st.Role] = true
		}
	}
	var roles []registry.SwarmRole
	if sw != nil {
		for _, r := range sw.Roles {
			if used[r.Name] {
				roles = append(roles, r)
			}
		}
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	raw, err := json.Marshal(struct {
		Workflow string               `json:"workflow"`
		Roles    []registry.SwarmRole `json:"roles"`
	}{wf.Hash(), roles})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// reuseFingerprint is the pair a hit must match: (reach, definition).
func (s *Server) reuseFingerprint(ctx context.Context, project *registry.Project, wf *registry.Workflow) (string, string, error) {
	reach, err := s.reuseReachHash(ctx, project.ID, wf.ID)
	if err != nil {
		return "", "", fmt.Errorf("reach hash: %w", err)
	}
	def, err := reuseDefinitionHash(wf, s.projectRegistry.GetSwarm(project.SwarmID))
	if err != nil {
		return "", "", fmt.Errorf("definition hash: %w", err)
	}
	return reach, def, nil
}

// tryReuse serves a hit when every check of §6b.4 holds; any failure is a
// miss (and drops the entry when it can never become a hit).
func (s *Server) tryReuse(ctx context.Context, key *persistence.APIKey, rk reuseKey, project *registry.Project, wf *registry.Workflow, maxAge int, notify *companionNotify) (string, bool) {
	cache := s.resultReuseState()
	e, ok := cache.get(rk)
	if !ok {
		recordResultReuse("miss_absent")
		return "", false
	}
	task, err := s.taskRepo.Get(ctx, e.taskID)
	if err != nil || task == nil {
		cache.drop(rk)
		recordResultReuse("miss_state")
		return "", false
	}
	// Check 1, defence in depth: the entry names a task this key created in
	// this project for this workflow.
	if task.ProjectID != rk.project || !keyCoversProject(key, task.ProjectID) ||
		derefString(task.WorkflowID) != rk.workflow || derefString(task.CreatedByAPIKeyID) != key.ID {
		cache.drop(rk)
		recordResultReuse("miss_state")
		return "", false
	}
	// Check 2: completed, and its answer validates.
	if task.Status != persistence.TaskStatusCompleted {
		if isTerminalStatus(task.Status) {
			cache.drop(rk)
		}
		recordResultReuse("miss_state")
		return "", false
	}
	if s.brokerStatusErrorClass(ctx, task, wf) != "" {
		cache.drop(rk)
		recordResultReuse("miss_state")
		return "", false
	}
	// Check 3: age from creation, so the data is at most that old. A
	// negative age (the task's clock ahead of this one: replica skew or a
	// rollback) passes, since the run is then as recent as it gets; the
	// answer reports it as 0 (review-20261008-82d8 F2).
	age := cache.now().Sub(task.CreatedAt)
	if age > time.Duration(maxAge)*time.Second {
		recordResultReuse("miss_expired")
		return "", false
	}
	// Checks 4 and 5: same reach, same definition.
	reach, def, err := s.reuseFingerprint(ctx, project, wf)
	if err != nil {
		// Unknown is not changed: a miss, and the entry stays for a later
		// call that can compute it.
		recordResultReuse("miss_reach")
		return "", false
	}
	if reach != e.reachHash {
		cache.drop(rk)
		recordResultReuse("miss_reach")
		return "", false
	}
	if def != e.definitionHash {
		cache.drop(rk)
		recordResultReuse("miss_definition")
		return "", false
	}
	if age < 0 {
		age = 0
	}
	out := map[string]any{
		"task_id":     task.ID,
		"status":      string(task.Status),
		"workflow":    wf.ID,
		"project":     project.ID,
		"reused":      true,
		"as_of":       task.CreatedAt.UTC().Format(time.RFC3339),
		"age_seconds": int(age / time.Second),
	}
	if notify != nil {
		out["push"] = "not_registered_reused"
	}
	recordResultReuse("hit")
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), true
}

// recordReuse stores the entry for a task this key just created. A
// fingerprint that cannot be computed records nothing.
func (s *Server) recordReuse(ctx context.Context, rk reuseKey, task *persistence.Task, project *registry.Project, wf *registry.Workflow) {
	reach, def, err := s.reuseFingerprint(ctx, project, wf)
	if err != nil {
		s.logger.Debug().Err(err).Str("workflow", wf.ID).Msg("result reuse: fingerprint unavailable; nothing recorded")
		return
	}
	cache := s.resultReuseState()
	cache.put(rk, reuseEntry{taskID: task.ID, reachHash: reach, definitionHash: def, recordedAt: cache.now()})
}

var (
	resultReuseMetricsOnce sync.Once
	resultReuseTotal       *prometheus.CounterVec
)

// recordResultReuse counts vornik_companion_result_reuse_total{outcome}
// (§6b.6): hit, miss_absent, miss_expired, miss_state, miss_reach,
// miss_definition, refused.
func recordResultReuse(outcome string) {
	resultReuseMetricsOnce.Do(func() {
		resultReuseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "companion", Name: "result_reuse_total",
			Help: "delegate calls with max_age_seconds, by outcome (hit, miss_absent, miss_expired, miss_state, miss_reach, miss_definition, refused).",
		}, []string{"outcome"})
	})
	resultReuseTotal.WithLabelValues(outcome).Inc()
}
