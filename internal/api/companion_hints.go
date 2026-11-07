package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The companion's just-in-time hint catalogue (GitHub #634; design
// 2026-08-20-companion-autonomy-and-failure-mode-hardening-design.md §13
// H1). One source: the daemon serves it, and both bundles embed the same
// content as hooks/hints.json (TestCompanionHints_BundleCopiesEqual_634).

//go:embed companion_hints.yaml
var companionHintsYAML []byte

// maxHintTextBytes bounds one hint's text, in bytes of UTF-8 (§13 H1).
const maxHintTextBytes = 300

// hintTriggerKinds is the fixed vocabulary the hooks detect (§13 H2). A new
// kind is added to the hooks before any hint uses it.
var hintTriggerKinds = map[string]bool{
	"commit":         true,
	"edits":          true,
	"design_written": true,
	"test_failed":    true,
	"first_prompt":   true,
}

// hintFollowRe is a follow target by logical name: recall, delegate:<workflow>
// or skill:<name>. Each bundle's hooks resolve it to their own tool names.
var hintFollowRe = regexp.MustCompile(`^(recall|delegate:[a-z0-9][a-z0-9-]*|skill:[a-z0-9][a-z0-9-]*)$`)

// hintPlaceholders are the names a hint's text may use; each bundle
// substitutes its own tool or command name.
var hintPlaceholderRe = regexp.MustCompile(`\{([a-z_]+)\}`)
var hintPlaceholders = map[string]bool{"recall": true, "delegate": true, "upload": true, "rag_ingest": true}

// HintTrigger is when a hint fires.
type HintTrigger struct {
	Kind    string   `yaml:"kind" json:"kind"`
	Globs   []string `yaml:"globs,omitempty" json:"globs,omitempty"`
	Runners []string `yaml:"runners,omitempty" json:"runners,omitempty"`
}

// CompanionHint is one catalogue entry.
type CompanionHint struct {
	ID      string      `yaml:"id" json:"id"`
	Version int         `yaml:"version" json:"version"`
	Trigger HintTrigger `yaml:"trigger" json:"trigger"`
	Follow  []string    `yaml:"follow" json:"follow"`
	Active  bool        `yaml:"active" json:"active"`
	Text    string      `yaml:"text" json:"text"`
}

// HintCatalogue is the whole catalogue.
type HintCatalogue struct {
	Hints []CompanionHint `yaml:"hints" json:"hints"`
}

// Validate refuses an entry the hooks could not act on.
func (c HintCatalogue) Validate() error {
	seen := map[string]bool{}
	for _, h := range c.Hints {
		switch {
		case strings.TrimSpace(h.ID) == "":
			return fmt.Errorf("a hint has no id")
		case seen[h.ID]:
			return fmt.Errorf("hint %s: duplicate id", h.ID)
		case h.Version < 1:
			return fmt.Errorf("hint %s: version must be 1 or more", h.ID)
		case !hintTriggerKinds[h.Trigger.Kind]:
			return fmt.Errorf("hint %s: unknown trigger kind %q", h.ID, h.Trigger.Kind)
		case h.Trigger.Kind == "design_written" && len(h.Trigger.Globs) == 0:
			return fmt.Errorf("hint %s: design_written needs globs", h.ID)
		case h.Trigger.Kind == "test_failed" && len(h.Trigger.Runners) == 0:
			return fmt.Errorf("hint %s: test_failed needs runners", h.ID)
		case len(h.Follow) == 0:
			return fmt.Errorf("hint %s: no follow target", h.ID)
		case strings.TrimSpace(h.Text) == "":
			return fmt.Errorf("hint %s: empty text", h.ID)
		case len(h.Text) > maxHintTextBytes:
			return fmt.Errorf("hint %s: text is %d bytes, at most %d", h.ID, len(h.Text), maxHintTextBytes)
		}
		follows := map[string]bool{}
		for _, f := range h.Follow {
			if !hintFollowRe.MatchString(f) {
				return fmt.Errorf("hint %s: follow target %q is not recall, delegate:<workflow> or skill:<name>", h.ID, f)
			}
			if follows[f] {
				return fmt.Errorf("hint %s: follow target %q listed twice", h.ID, f)
			}
			follows[f] = true
		}
		for _, m := range hintPlaceholderRe.FindAllStringSubmatch(h.Text, -1) {
			if !hintPlaceholders[m[1]] {
				return fmt.Errorf("hint %s: unknown placeholder {%s}", h.ID, m[1])
			}
		}
		seen[h.ID] = true
	}
	return nil
}

var (
	companionHintsOnce sync.Once
	companionHintsCat  HintCatalogue
	companionHintsErr  error
)

// CompanionHints returns the embedded catalogue, validated once: it is fixed
// at build time (review 857b, minor).
func CompanionHints() (HintCatalogue, error) {
	companionHintsOnce.Do(func() {
		var c HintCatalogue
		if err := yaml.Unmarshal(companionHintsYAML, &c); err != nil {
			companionHintsErr = fmt.Errorf("companion hints: %w", err)
			return
		}
		if err := c.Validate(); err != nil {
			companionHintsErr = fmt.Errorf("companion hints: %w", err)
			return
		}
		companionHintsCat = c
	})
	return companionHintsCat, companionHintsErr
}

// CompanionHintsJSON is the catalogue as the bundles embed it.
func CompanionHintsJSON() ([]byte, error) {
	c, err := CompanionHints()
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// hintsResponse is what GET /api/v1/companion/hints serves (§13 H1, H3).
// Hints are the active, unmuted ones; Muted is the project's mask, which the
// hooks save for their offline fallback (§13 H3).
type hintsResponse struct {
	Project string          `json:"project"`
	Enabled bool            `json:"enabled"`
	Hints   []CompanionHint `json:"hints"`
	Muted   []string        `json:"muted"`
}

// requestKeyRow is the request's DB key row, or nil.
func requestKeyRow(r *http.Request) *persistence.APIKey {
	if id := IdentityFromContext(r.Context()); id != nil && id.Extra != nil {
		row, _ := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
		return row
	}
	return nil
}

// companionKeyRow is the request's companion key, or nil. A key with a
// client kind is exactly what AuthMiddleware confines to the companion paths
// (isCompanionAllowedPathFor), so this matches the gate that opened the route
// (review 857b F1).
func companionKeyRow(r *http.Request) *persistence.APIKey {
	row := requestKeyRow(r)
	if row == nil || row.ClientKind == "" || row.ProjectID == "" {
		return nil
	}
	return row
}

// backedOffHints is the project's hints whose back-off runs at now. A store
// error is logged and read as none: the catalogue is served without the
// back-off rather than not at all (§13.10 S5).
func (s *Server) backedOffHints(r *http.Request, projectID string, now time.Time) (map[string]persistence.HintWindow, map[string]bool) {
	off := map[string]bool{}
	if s.companionHintRepo == nil {
		return nil, off
	}
	windows, err := s.companionHintRepo.Windows(r.Context(), projectID)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("companion hints: windows unreadable; serving without back-off")
		return nil, off
	}
	for id, w := range windows {
		if w.BackedOff(now) {
			off[id] = true
		}
	}
	return windows, off
}

// CompanionHintsGet serves the hint catalogue to a companion key for the
// key's own project (#634). The project comes from the key, never from the
// request. Backed-off hints are left out and added to the mask (§13.10 S5).
func (s *Server) CompanionHintsGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	row := companionKeyRow(r)
	if row == nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "the hint catalogue is served to companion keys")
		return
	}
	cat, err := CompanionHints()
	if err != nil {
		s.logger.Error().Err(err).Msg("companion hints: the embedded catalogue does not load")
		respondError(w, http.StatusInternalServerError, "INTERNAL", "the hint catalogue is unavailable")
		return
	}
	resp := hintsResponse{Project: row.ProjectID, Enabled: true, Hints: []CompanionHint{}, Muted: []string{}}
	var project *registry.Project
	if s.projectRegistry != nil {
		project = s.projectRegistry.GetProject(row.ProjectID)
	}
	muted := map[string]bool{}
	if project != nil {
		resp.Enabled = project.HintsEnabled()
		for _, id := range project.Companion.Mute {
			muted[id] = true
			if resp.Enabled {
				// With hints off nothing is served, so no mask either
				// (review 857b F3).
				resp.Muted = append(resp.Muted, id)
			}
		}
	}
	now := time.Now()
	if resp.Enabled {
		_, backedOff := s.backedOffHints(r, row.ProjectID, now)
		for _, h := range cat.Hints {
			switch {
			case !h.Active || muted[h.ID]:
			case backedOff[h.ID]:
				resp.Muted = append(resp.Muted, h.ID)
			default:
				resp.Hints = append(resp.Hints, h)
			}
		}
	}
	if s.companionHintRepo != nil {
		if err := s.companionHintRepo.TouchFetch(r.Context(), row.ID, row.ProjectID, now); err != nil {
			s.logger.Warn().Err(err).Str("key", row.ID).Msg("companion hints: last fetch not recorded")
		}
	}
	respondJSON(w, http.StatusOK, resp)
}

// The upload's caps (§13.10 S3).
const (
	maxHintOutcomeBytes  = 64 << 10
	maxHintOutcomeEvents = 500
)

type hintOutcomeEvent struct {
	ID       string `json:"id"`
	Followed bool   `json:"followed"`
}

type hintOutcomesRequest struct {
	Events []hintOutcomeEvent `json:"events"`
}

type hintOutcomesResponse struct {
	Accepted       int `json:"accepted"`
	DroppedBackoff int `json:"dropped_backoff"`
	Unknown        int `json:"unknown"`
}

// CompanionHintOutcomesPost records, for the companion key's own project,
// whether each hint firing was followed (#634 slice 2; design §13 H4,
// §13.10 S3). The body is ids and booleans only; unknown fields are refused.
func (s *Server) CompanionHintOutcomesPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	row := companionKeyRow(r)
	if row == nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "hint outcomes are accepted from companion keys")
		return
	}
	if s.companionHintRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "hint outcomes are not stored on this daemon")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxHintOutcomeBytes+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "the body could not be read")
		return
	}
	if len(raw) > maxHintOutcomeBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", fmt.Sprintf("the body is over %d bytes", maxHintOutcomeBytes))
		return
	}
	var req hintOutcomesRequest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	// One object, with an events list, and nothing after it (review c48a F2).
	if err := dec.Decode(&req); err != nil || dec.More() || req.Events == nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "the body is {\"events\":[{\"id\":…,\"followed\":…}]} and nothing else")
		return
	}
	if len(req.Events) > maxHintOutcomeEvents {
		respondError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", fmt.Sprintf("at most %d events per upload", maxHintOutcomeEvents))
		return
	}
	cat, err := CompanionHints()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "the hint catalogue is unavailable")
		return
	}
	known := map[string]bool{}
	for _, h := range cat.Hints {
		known[h.ID] = true
	}
	// Group by id, keeping body order within each hint.
	var order []string
	byID := map[string][]bool{}
	var resp hintOutcomesResponse
	for _, e := range req.Events {
		if !known[e.ID] {
			resp.Unknown++
			continue
		}
		if _, seen := byID[e.ID]; !seen {
			order = append(order, e.ID)
		}
		byID[e.ID] = append(byID[e.ID], e.Followed)
	}
	now := time.Now()
	for _, id := range order {
		_, applied, err := s.companionHintRepo.Apply(r.Context(), row.ProjectID, id, byID[id], now)
		if errors.Is(err, persistence.ErrHintWindowConflict) {
			// The hook keeps the session for its next turn (H4's 2xx gate).
			respondError(w, http.StatusServiceUnavailable, "CONFLICT_RETRY", "concurrent uploads; send again later")
			return
		}
		if err != nil {
			s.logger.Error().Err(err).Str("project", row.ProjectID).Str("hint", id).Msg("companion hints: outcome not stored")
			respondError(w, http.StatusInternalServerError, "DB_ERROR", "hint outcomes could not be stored")
			return
		}
		resp.Accepted += applied
		resp.DroppedBackoff += len(byID[id]) - applied
	}
	if err := s.companionHintRepo.TouchUpload(r.Context(), row.ID, row.ProjectID, now); err != nil {
		s.logger.Warn().Err(err).Str("key", row.ID).Msg("companion hints: last upload not recorded")
	}
	respondJSON(w, http.StatusOK, resp)
}

// hintReportRow is one catalogue hint in the operator report (§13.10 S6).
type hintReportRow struct {
	ID            string     `json:"id"`
	Trigger       string     `json:"trigger"`
	State         string     `json:"state"`
	BackoffUntil  *time.Time `json:"backoff_until,omitempty"`
	Window        string     `json:"window"`
	FollowRate    *float64   `json:"follow_rate,omitempty"`
	FiredTotal    int        `json:"fired_total"`
	FollowedTotal int        `json:"followed_total"`
}

type hintReportClient struct {
	KeyID        string     `json:"key_id"`
	LastFetchAt  *time.Time `json:"last_fetch_at,omitempty"`
	LastUploadAt *time.Time `json:"last_upload_at,omitempty"`
}

type hintReport struct {
	Project string             `json:"project"`
	Enabled bool               `json:"enabled"`
	Hints   []hintReportRow    `json:"hints"`
	Clients []hintReportClient `json:"clients"`
}

// CompanionHintsReport lists every catalogue hint with its state, window and
// counts for one project, and the project's companion keys' last contact
// (#634 slice 2; §13.10 S6). Operator-scoped, like /api/v1/companion/keys;
// not on the companion allowlist, and refused to a companion key here too.
func (s *Server) CompanionHintsReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if row := requestKeyRow(r); row != nil && row.ClientKind != "" {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "the hint report is for operators")
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId query parameter is required")
		return
	}
	var project *registry.Project
	if s.projectRegistry != nil {
		project = s.projectRegistry.GetProject(projectID)
	}
	if project == nil || !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found: "+projectID)
		return
	}
	cat, err := CompanionHints()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "the hint catalogue is unavailable")
		return
	}
	now := time.Now()
	windows, _ := s.backedOffHints(r, projectID, now)
	muted := map[string]bool{}
	for _, id := range project.Companion.Mute {
		muted[id] = true
	}
	rep := hintReport{Project: projectID, Enabled: project.HintsEnabled(), Hints: []hintReportRow{}, Clients: []hintReportClient{}}
	for _, h := range cat.Hints {
		win := windows[h.ID]
		row := hintReportRow{ID: h.ID, Trigger: h.Trigger.Kind, Window: win.Window, FiredTotal: win.FiredTotal, FollowedTotal: win.FollowedTotal}
		if n := len(win.Window); n > 0 {
			rate := float64(strings.Count(win.Window, "1")) / float64(n)
			row.FollowRate = &rate
		}
		// The first state that holds (§13.10 S6).
		switch {
		case !rep.Enabled:
			row.State = "off"
		case muted[h.ID]:
			row.State = "muted"
		case !h.Active:
			row.State = "inactive"
		case win.BackedOff(now):
			row.State = "backed_off"
			row.BackoffUntil = win.BackoffUntil
		default:
			row.State = "active"
		}
		rep.Hints = append(rep.Hints, row)
	}
	if s.companionHintRepo != nil {
		clients, err := s.companionHintRepo.Clients(r.Context(), projectID)
		if err != nil {
			s.logger.Warn().Err(err).Str("project", projectID).Msg("companion hints: clients unreadable")
		}
		for _, c := range clients {
			rep.Clients = append(rep.Clients, hintReportClient{KeyID: c.KeyID, LastFetchAt: c.LastFetchAt, LastUploadAt: c.LastUploadAt})
		}
	}
	respondJSON(w, http.StatusOK, rep)
}
