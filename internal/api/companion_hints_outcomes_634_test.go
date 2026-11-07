package api

// GitHub #634 slice 2 (companion hardening design §13.10): the outcome
// upload, the back-off on the catalogue GET, and the operator report.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func hintsServer(t *testing.T, companionYAML string) (*Server, persistence.CompanionHintRepository) {
	t.Helper()
	repo := sqlite.NewCompanionHintRepository(sqlitetest.Memory(t).DB)
	return &Server{logger: zerolog.Nop(), projectRegistry: hintsRegistry(t, companionYAML), companionHintRepo: repo}, repo
}

func withKey(r *http.Request, row *persistence.APIKey) *http.Request {
	if row == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}}))
}

func postOutcomes(s *Server, row *persistence.APIKey, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := withKey(httptest.NewRequest(http.MethodPost, "/api/v1/companion/hints/outcomes", strings.NewReader(body)), row)
	s.CompanionHintOutcomesPost(w, r)
	return w
}

type outcomesBody struct {
	Accepted       int `json:"accepted"`
	DroppedBackoff int `json:"dropped_backoff"`
	Unknown        int `json:"unknown"`
}

// eventsJSON is one commit.review event per character of pattern, '1' =
// followed.
func eventsJSON(pattern string) string {
	var parts []string
	for _, c := range pattern {
		parts = append(parts, fmt.Sprintf(`{"id":"commit.review","followed":%t}`, c == '1'))
	}
	return strings.Join(parts, ",")
}

func TestCompanionHintOutcomes_AppliesForKeysProject_634(t *testing.T) {
	s, repo := hintsServer(t, "")
	w := postOutcomes(s, companionRow(), `{"events":[`+eventsJSON("10")+`,{"id":"no.such","followed":true}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var b outcomesBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	require.Equal(t, outcomesBody{Accepted: 2, Unknown: 1}, b)
	got, err := repo.Windows(context.Background(), "proj")
	require.NoError(t, err)
	require.Equal(t, "10", got["commit.review"].Window)
	require.NotContains(t, got, "no.such", "an id outside the catalogue makes no row")
	clients, err := repo.Clients(context.Background(), "proj")
	require.NoError(t, err)
	require.Len(t, clients, 1)
	require.NotNil(t, clients[0].LastUploadAt)
	other, err := repo.Windows(context.Background(), "other")
	require.NoError(t, err)
	require.Empty(t, other)
}

// The body is the key's own project's events only: a project field, or any
// field besides events, is refused (§13.10 S3).
func TestCompanionHintOutcomes_RefusesUnknownFields_634(t *testing.T) {
	s, repo := hintsServer(t, "")
	for _, body := range []string{
		`{"project":"other","events":[]}`,
		`{"events":[{"id":"commit.review","followed":true,"text":"x"}]}`,
		`not json`,
		`{"events":[]}{"events":[]}`, // trailing data (review c48a F2)
		`null`,
		`{}`,
	} {
		require.Equal(t, http.StatusBadRequest, postOutcomes(s, companionRow(), body).Code, body)
	}
	got, _ := repo.Windows(context.Background(), "proj")
	require.Empty(t, got)
}

func TestCompanionHintOutcomes_Caps_634(t *testing.T) {
	s, _ := hintsServer(t, "")
	body := `{"events":[` + eventsJSON(strings.Repeat("1", 501)) + `]}`
	require.Equal(t, http.StatusRequestEntityTooLarge, postOutcomes(s, companionRow(), body).Code, "501 events")
	big := `{"events":[],"pad":"` + strings.Repeat("x", 70<<10) + `"}`
	require.Equal(t, http.StatusRequestEntityTooLarge, postOutcomes(s, companionRow(), big).Code, "over 64 KiB")
	body = `{"events":[` + eventsJSON(strings.Repeat("1", 500)) + `]}`
	require.Equal(t, http.StatusOK, postOutcomes(s, companionRow(), body).Code, "500 events")
}

func TestCompanionHintOutcomes_BackoffDropsAreCounted_634(t *testing.T) {
	s, _ := hintsServer(t, "")
	w := postOutcomes(s, companionRow(), `{"events":[`+eventsJSON("000000000011")+`]}`)
	require.Equal(t, http.StatusOK, w.Code)
	var b outcomesBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	require.Equal(t, outcomesBody{Accepted: 10, DroppedBackoff: 2}, b)
}

func TestCompanionHintOutcomes_RefusesNonCompanionKeys_634(t *testing.T) {
	s, _ := hintsServer(t, "")
	require.Equal(t, http.StatusForbidden, postOutcomes(s, &persistence.APIKey{ID: "k2", ProjectID: "proj"}, `{"events":[]}`).Code)
	require.Equal(t, http.StatusForbidden, postOutcomes(s, nil, `{"events":[]}`).Code)
	s.companionHintRepo = nil
	require.Equal(t, http.StatusServiceUnavailable, postOutcomes(s, companionRow(), `{"events":[]}`).Code)
}

type conflictHintRepo struct {
	persistence.CompanionHintRepository
}

func (conflictHintRepo) Apply(context.Context, string, string, []bool, time.Time) (persistence.HintWindow, int, error) {
	return persistence.HintWindow{}, 0, persistence.ErrHintWindowConflict
}

// Lost compare-and-sets answer 503, so the hook keeps the session (S2).
func TestCompanionHintOutcomes_ConflictIs503_634(t *testing.T) {
	s, repo := hintsServer(t, "")
	s.companionHintRepo = conflictHintRepo{repo}
	require.Equal(t, http.StatusServiceUnavailable, postOutcomes(s, companionRow(), `{"events":[`+eventsJSON("1")+`]}`).Code)
}

// A backed-off hint is left out of the catalogue and added to the mask, and
// the fetch is recorded (S4, S5).
func TestCompanionHintsGet_LeavesOutBackedOff_634(t *testing.T) {
	s, repo := hintsServer(t, "")
	_, _, err := repo.Apply(context.Background(), "proj", "test_failed.recall", make([]bool, 10), time.Now())
	require.NoError(t, err)
	code, b := getHints(t, s, companionRow())
	require.Equal(t, http.StatusOK, code)
	for _, h := range b.Hints {
		require.NotEqual(t, "test_failed.recall", h.ID, "a backed-off hint was served")
	}
	require.Contains(t, b.Muted, "test_failed.recall")
	clients, err := repo.Clients(context.Background(), "proj")
	require.NoError(t, err)
	require.Len(t, clients, 1)
	require.NotNil(t, clients[0].LastFetchAt)
}

type failingHintRepo struct {
	persistence.CompanionHintRepository
}

func (failingHintRepo) Windows(context.Context, string) (map[string]persistence.HintWindow, error) {
	return nil, errors.New("db down")
}
func (failingHintRepo) TouchFetch(context.Context, string, string, time.Time) error {
	return errors.New("db down")
}

// A store failure serves the catalogue without the back-off (S5).
func TestCompanionHintsGet_StoreFailureStillServes_634(t *testing.T) {
	s, repo := hintsServer(t, "")
	s.companionHintRepo = failingHintRepo{repo}
	code, b := getHints(t, s, companionRow())
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, b.Hints)
}

func getReport(s *Server, row *persistence.APIKey, project string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/companion/hints/report?projectId="+project, nil)
	r = withKey(r, row)
	s.CompanionHintsReport(w, r)
	return w
}

// The report: every catalogue hint with its state, first match wins (S6).
func TestCompanionHintsReport_States_634(t *testing.T) {
	s, repo := hintsServer(t, "companion:\n  mute: [\"commit.review\", \"first_prompt.recall\"]\n")
	ctx := context.Background()
	_, _, err := repo.Apply(ctx, "proj", "test_failed.recall", make([]bool, 10), time.Now())
	require.NoError(t, err)
	_, _, err = repo.Apply(ctx, "proj", "commit.review", make([]bool, 10), time.Now())
	require.NoError(t, err)
	_, _, err = repo.Apply(ctx, "proj", "design_written.ingest", []bool{true, false}, time.Now())
	require.NoError(t, err)
	require.NoError(t, repo.TouchFetch(ctx, "k1", "proj", time.Now()))

	w := getReport(s, nil, "proj")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var rep hintReport
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	cat, _ := CompanionHints()
	require.Len(t, rep.Hints, len(cat.Hints), "every catalogue hint is listed")
	state := map[string]hintReportRow{}
	for _, h := range rep.Hints {
		state[h.ID] = h
	}
	require.Equal(t, "muted", state["commit.review"].State, "muted wins over backed off")
	require.Equal(t, "muted", state["first_prompt.recall"].State, "muted wins over inactive")
	require.Equal(t, "backed_off", state["test_failed.recall"].State)
	require.NotNil(t, state["test_failed.recall"].BackoffUntil)
	require.Equal(t, "inactive", state["edits.recall_design"].State)
	ingest := state["design_written.ingest"]
	require.Equal(t, "active", ingest.State)
	require.Equal(t, "10", ingest.Window)
	require.Equal(t, 2, ingest.FiredTotal)
	require.Equal(t, 1, ingest.FollowedTotal)
	require.Len(t, rep.Clients, 1)
	require.Equal(t, "k1", rep.Clients[0].KeyID)

	off, _ := hintsServer(t, "companion:\n  hints: \"off\"\n")
	w = getReport(off, nil, "proj")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	for _, h := range rep.Hints {
		require.Equal(t, "off", h.State)
	}
}

// A companion key cannot read the report, and an unknown project is 404.
func TestCompanionHintsReport_Access_634(t *testing.T) {
	s, _ := hintsServer(t, "")
	require.Equal(t, http.StatusForbidden, getReport(s, companionRow(), "proj").Code)
	require.Equal(t, http.StatusNotFound, getReport(s, nil, "nope").Code)
	require.Equal(t, http.StatusBadRequest, getReport(s, nil, "").Code)
}
