package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
)

// WizardPageData is the template payload for /ui/projects/new/wizard.
// The page is otherwise static — per-turn state lives client-side in JS
// that calls /api/v1/projects/wizard/converse. When the operator
// resumes an existing draft (banner "Resume" link → ?session=<id>),
// ResumeSessionID seeds the client's sessionID so the page continues
// that session (Converse appends to it; Cancel targets it) instead of
// silently starting a fresh one — and ResumeTranscript replays the
// prior turns into the chat pane.
type WizardPageData struct {
	Title       string
	CurrentPage string

	// ResumeSessionID is the draft being resumed ("" for a new wizard).
	ResumeSessionID string
	// ResumeTranscript is the prior turns, {role,content} each. The page's
	// wizard-data block writes it with html/template, which escapes it as
	// JSON (#416 phase 5: no data block holds a template.JS). Never nil, so
	// it renders as [] when there is nothing to replay.
	ResumeTranscript []wizardTurn
}

// ProjectsNewWizard renders the conversational project-setup wizard.
// With ?session=<id> it resumes one of the requesting operator's own
// uncommitted drafts.
func (s *Server) ProjectsNewWizard(w http.ResponseWriter, r *http.Request) {
	// GitHub #560 (M1b B2b): tenant_admin passes this check on purpose: project creation is granted to the instance owner.
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := WizardPageData{
		Title:            "New project — wizard",
		CurrentPage:      "projects",
		ResumeTranscript: []wizardTurn{},
	}
	if sid := strings.TrimSpace(r.URL.Query().Get("session")); sid != "" {
		s.populateWizardResume(r, &data, sid)
	}
	s.render(w, "projects_new_wizard.html", data)
}

// populateWizardResume seeds data for a resumed draft, but ONLY when
// sid is one of the requesting operator's own uncommitted, un-cancelled
// sessions. An unknown / foreign / committed / cancelled id is silently
// ignored (the page renders fresh) so a crafted ?session= can neither
// resume another operator's draft nor revive a closed one.
func (s *Server) populateWizardResume(r *http.Request, data *WizardPageData, sid string) {
	if s.wizardSessions == nil {
		return
	}
	operator := s.operatorIDForRequest(r)
	if operator == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rows, err := s.wizardSessions.ListByOperator(ctx, operator, 100)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row == nil || row.ID != sid {
			continue
		}
		if row.CommittedProjectID != nil || row.CancelledAt != nil {
			return // closed draft — nothing to resume
		}
		data.ResumeSessionID = row.ID
		data.ResumeTranscript = resumeTranscript(row.Transcript)
		return
	}
}

// wizardTurn is one replayed turn of a resumed wizard draft.
type wizardTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// resumeTranscript projects a stored wizard transcript down to its
// {role,content} turns for replay. Returns an empty list on empty or
// invalid input rather than failing the page render.
func resumeTranscript(raw []byte) []wizardTurn {
	out := []wizardTurn{}
	if len(raw) == 0 {
		return out
	}
	var turns []wizardTurn
	if err := json.Unmarshal(raw, &turns); err != nil {
		return out
	}
	for _, t := range turns {
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		out = append(out, wizardTurn{Role: t.Role, Content: t.Content})
	}
	return out
}
