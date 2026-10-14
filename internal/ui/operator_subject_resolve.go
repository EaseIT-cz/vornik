package ui

// /ui/operator/regulatory/requests/<id>/resolve — data-subject resolution in
// the console (GitHub #345). Knowledge-graph entity candidates (the CLI's
// `subject resolve-kg`, unchanged underneath) and the §5.1 free-text backstop,
// for a VERIFIED rights request only, with every search audited before it reads
// and every decision audited after it writes.
//
// Unlike the regulatory record, this page shows personal data (candidate names
// and bounded snippets) because the judgement it asks for cannot be made blind:
// it is no-store / no-referrer, its search terms travel only in POST bodies, and
// nothing on it can be downloaded.
//
// see LLD § https://docs.vornik.io §4.17

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/subjectresolve"
)

// SubjectResolveKG is the knowledge-graph half: *datasubject.KGResolver.
type SubjectResolveKG interface {
	Candidates(ctx context.Context, subjectID string, names, projects []string) ([]datasubject.KGCandidate, error)
	Bind(ctx context.Context, req datasubject.KGBindRequest) (datasubject.KGBindResult, error)
}

// SubjectResolveLinks is the subject-axis surface the page reads and writes:
// *postgres.DataSubjectRepository.
type SubjectResolveLinks interface {
	GetSubject(ctx context.Context, id string) (datasubject.Subject, error)
	ListIdentifiers(ctx context.Context, subjectID string) ([]datasubject.Identifier, error)
	ListLinks(ctx context.Context, subjectID string) ([]datasubject.Link, error)
	AddLinkIfAbsent(ctx context.Context, subjectID string, l datasubject.Link) (bool, error)
	DeleteOperatorLink(ctx context.Context, subjectID, table, rowID string) (bool, error)
}

// WithSubjectResolution wires the resolution page. Any nil dependency leaves it
// "not recorded on this deployment" — never a partial page. The request ledger
// and the audit sink come from WithRegulatoryLedgers.
func WithSubjectResolution(kg SubjectResolveKG, links SubjectResolveLinks,
	text persistence.SubjectTextSearch, screen persistence.RestrictionScreen) ServerOption {
	return func(s *Server) {
		s.resolveKG, s.resolveLinks, s.resolveText, s.resolveScreen = kg, links, text, screen
		s.resolveLimiter = newSearchLimiter(resolveSearchesPerWindow, resolveSearchWindow)
	}
}

const (
	resolveTextCap           = 50
	resolveKGCap             = 50
	resolveSnippetRunes      = 80
	resolveSearchesPerWindow = 20
	resolveSearchWindow      = 10 * time.Minute
	resolveMaxDecisions      = 200
)

// resolveService wires the gates shared with `vornikctl subject resolve-kg`
// (design §4.17.9). A nil audit sink stays nil: every search is then refused.
func (s *Server) resolveService() *subjectresolve.Service {
	svc := &subjectresolve.Service{Requests: s.regRequests, KG: s.resolveKG, Screen: s.resolveScreen}
	if s.regAudit != nil {
		svc.Audit = s.regAudit
	}
	return svc
}

// resolveActor is the audit identity of a console request.
func (s *Server) resolveActor(r *http.Request) subjectresolve.Actor {
	return subjectresolve.Actor{Principal: adminPrincipal(r), Source: "ui", IP: clientIP(r), UserAgent: r.UserAgent()}
}

func (s *Server) resolveWired() bool {
	return s.regRequests != nil && s.resolveKG != nil && s.resolveLinks != nil &&
		s.resolveText != nil && s.resolveScreen != nil
}

// resolveKGRow is one knowledge-graph candidate as rendered.
type resolveKGRow struct {
	EntityID, ProjectID, Name, Aliases, MatchedOn, State, BoundSubject string
	Mentions                                                           int
	Withheld, CanConfirm, Placeholder                                  bool
}

// resolveTextRow is one free-text candidate as rendered.
type resolveTextRow struct {
	ChunkID, ProjectID, Created string
	Pre, Match, Post            string
	SnippetWithheld             bool
	LinkedSource                string
	OtherSubjects               int
	CanConfirm, CanUnlink       bool
}

// OperatorSubjectResolveData backs operator_subject_resolve.html.
type OperatorSubjectResolveData struct {
	adminCommonData
	NotRecorded          bool
	RequestID, SubjectID string
	Right, SubjectName   string
	IsRestriction        bool
	KGBound              bool
	OperatorLinks        int
	Names                []string
	Project              string
	Searched             bool
	KGWithheld           string
	KG                   []resolveKGRow
	KGCapped             bool
	Text                 []resolveTextRow
	TextCapped           bool
	Outcomes             []string
	Errors               []string
}

// operatorSubjectResolve serves GET (the form) and POST (search / apply).
// The caller (operatorRegulatoryRouter) has already applied the operator gate.
func (s *Server) operatorSubjectResolve(w http.ResponseWriter, r *http.Request, requestID string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	data := OperatorSubjectResolveData{
		adminCommonData: adminCommonData{Title: "Resolve data subject", CurrentPage: "regulatory", IsAdmin: true},
		RequestID:       requestID,
	}
	if !s.resolveWired() {
		if r.Method == http.MethodPost {
			http.Error(w, "not available on this deployment: subject resolution needs the Postgres knowledge-graph subject index", http.StatusNotFound)
			return
		}
		data.NotRecorded = true
		s.render(w, "operator_subject_resolve.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Purpose limitation: a name search over everything the deployment holds is
	// lawful here only because a VERIFIED rights request requires it. The gate
	// is shared with `vornikctl subject resolve-kg` (design §4.17.9).
	svc := s.resolveService()
	req, err := svc.Authorize(ctx, requestID)
	var notVerified *subjectresolve.NotVerifiedError
	switch {
	case errors.Is(err, subjectresolve.ErrRequestNotFound):
		http.Error(w, "no such request", http.StatusNotFound)
		return
	case errors.As(err, &notVerified):
		http.Error(w, notVerified.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logger.Warn().Err(err).Str("request_id", requestID).Msg("subject resolve: request read failed")
		http.Error(w, "the request could not be read", http.StatusInternalServerError)
		return
	}
	subject, err := s.resolveLinks.GetSubject(ctx, req.SubjectID)
	if err != nil {
		s.logger.Warn().Err(err).Str("subject_id", req.SubjectID).Msg("subject resolve: subject read failed")
		http.Error(w, "the subject could not be read", http.StatusInternalServerError)
		return
	}
	data.SubjectID, data.SubjectName = req.SubjectID, subject.DisplayName
	data.Right = string(req.Kind)
	if art, ok := requestArticle[req.Kind]; ok {
		data.Right = art + " " + data.Right
	}
	data.IsRestriction = req.Kind == datasubject.RequestRestriction

	if r.Method == http.MethodPost {
		if status, msg := s.resolvePost(ctx, r, svc, req, subject, &data); status != http.StatusOK {
			http.Error(w, msg, status)
			return
		}
	}
	s.resolveCoverage(ctx, req.SubjectID, &data)
	s.render(w, "operator_subject_resolve.html", data)
}

// resolvePost runs one search (phase 1, audited first) and, for "apply", the
// operator's decisions (phase 2, each audited after its write), then renders
// the fresh candidate lists. A non-200 status means nothing was read or
// written.
func (s *Server) resolvePost(ctx context.Context, r *http.Request, svc *subjectresolve.Service, req datasubject.Request,
	subject datasubject.Subject, data *OperatorSubjectResolveData) (int, string) {
	if err := r.ParseForm(); err != nil {
		return http.StatusBadRequest, "bad form"
	}
	names, err := subjectresolve.ValidateNames(r.Form["name"])
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}
	data.Names = names
	data.Project = strings.TrimSpace(r.FormValue("project"))
	terms := subjectresolve.SearchTerms(subject.DisplayName, names)

	// Rate first: a refused search writes no audit row and reads nothing.
	if !s.resolveLimiter.allow(adminPrincipal(r), time.Now()) {
		return http.StatusTooManyRequests, fmt.Sprintf(
			"search limit reached: at most %d resolution searches per %s", resolveSearchesPerWindow, resolveSearchWindow)
	}
	// Phase 1: the search is on record BEFORE anything is read.
	var projects []string
	if data.Project != "" {
		projects = []string{data.Project}
	}
	if err := svc.AuditSearch(ctx, s.resolveActor(r), req, projects, terms); errors.Is(err, subjectresolve.ErrNoAudit) {
		return http.StatusServiceUnavailable, "searches are unavailable: no audit log is wired, and an unaudited search is refused"
	} else if err != nil {
		s.logger.Warn().Err(err).Str("request_id", req.ID).Msg("subject resolve: search refused — audit write failed")
		return http.StatusInternalServerError, "the search could not be recorded, so it was refused"
	}
	data.Searched = true

	// Phase 2: decisions, re-checked against fresh reads.
	if r.FormValue("action") == "apply" {
		s.resolveApply(ctx, r, svc, req, terms, data)
		if ctx.Err() != nil {
			// A long apply ran out of time: later decisions were refused,
			// not silently skipped — say so once (code review finding 4).
			data.Errors = append(data.Errors, "The request timed out while applying decisions; "+
				"decisions after the timeout were refused. Check the outcomes and apply the rest again.")
		}
	}

	s.resolveKGCandidates(ctx, svc, req.SubjectID, names, projects, data)
	s.resolveTextCandidates(ctx, req.SubjectID, terms, data)
	return http.StatusOK, ""
}

// resolveDecision is one operator choice from the form.
type resolveDecision struct{ kind, id, choice string }

func parseDecisions(r *http.Request) []resolveDecision {
	var out []resolveDecision
	for key, vals := range r.PostForm {
		if len(vals) == 0 {
			continue
		}
		kind, id, ok := strings.Cut(key, ":")
		if !ok || id == "" || (kind != "kg" && kind != "chunk") {
			continue
		}
		choice := vals[0]
		if choice == "" || choice == "leave" {
			continue
		}
		out = append(out, resolveDecision{kind: kind, id: id, choice: choice})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].id < out[j].id
	})
	if len(out) > resolveMaxDecisions {
		out = out[:resolveMaxDecisions]
	}
	return out
}

// resolveApply executes the operator's decisions. Each is re-checked against a
// fresh read and audited AFTER its write with the outcome, so a row never
// claims a write that did not happen.
func (s *Server) resolveApply(ctx context.Context, r *http.Request, svc *subjectresolve.Service, req datasubject.Request, terms []string, data *OperatorSubjectResolveData) {
	for _, d := range parseDecisions(r) {
		action, outcome, audited := s.applyDecision(ctx, svc, req, terms, data, d)
		data.Outcomes = append(data.Outcomes, fmt.Sprintf("%s %s: %s", d.kind, d.id, outcome))
		if err := svc.AuditDecision(ctx, s.resolveActor(r), action, req, map[string]any{
			"subject_id": req.SubjectID, "kind": d.kind, "id": d.id, "choice": d.choice, "outcome": audited,
		}); err != nil {
			s.logger.Warn().Err(err).Str("request_id", req.ID).Str("action", action).Msg("subject resolve: decision audit write failed")
			data.Outcomes = append(data.Outcomes, fmt.Sprintf("%s %s: AUDIT WRITE FAILED — see the daemon log", d.kind, d.id))
		}
	}
}

func (s *Server) applyDecision(ctx context.Context, svc *subjectresolve.Service, req datasubject.Request, terms []string,
	data *OperatorSubjectResolveData, d resolveDecision) (action, outcome, audited string) {
	switch {
	case d.choice == "reject":
		return "subject.resolve.reject", "ok", "ok"
	case d.kind == "kg" && (d.choice == "confirm" || d.choice == "adopt"):
		var projects []string
		if data.Project != "" {
			projects = []string{data.Project}
		}
		// Screen then bind, shared with the CLI (design §4.17.9). The page
		// shows Bind's full refusal (it may quote another subject's display
		// name); admin_audit gets only the outcome class (§4.17.8 item 2).
		res := svc.BindEntity(ctx, req, d.id, data.Names, projects, d.choice == "adopt")
		return subjectresolve.ActionBindEntity, res.Page, res.Audited
	case d.kind == "chunk" && d.choice == "confirm":
		out := s.linkChunkDecision(ctx, req, terms, data, d.id)
		return "subject.resolve.link_chunk", out, out
	case d.kind == "chunk" && d.choice == "unlink":
		out := s.unlinkChunkDecision(ctx, req, d.id)
		return "subject.resolve.unlink_chunk", out, out
	}
	out := "refused: unknown choice"
	return "subject.resolve.reject", out, out
}

// unlinkChunkDecision removes an operator-asserted link only.
func (s *Server) unlinkChunkDecision(ctx context.Context, req datasubject.Request, chunkID string) string {
	removed, err := s.resolveLinks.DeleteOperatorLink(ctx, req.SubjectID, string(datasubject.TableProjectMemoryChunks), chunkID)
	switch {
	case err != nil:
		return "refused: the link could not be removed"
	case !removed:
		return "refused: no operator-asserted link to remove"
	}
	return "ok"
}

// linkChunkDecision re-checks a free-text candidate and links it at
// operator_asserted/probable/unknown, never over an existing link.
func (s *Server) linkChunkDecision(ctx context.Context, req datasubject.Request, terms []string, data *OperatorSubjectResolveData, chunkID string) string {
	m, found, err := s.recheckChunk(ctx, req.SubjectID, chunkID, terms, data.Project)
	switch {
	case err != nil:
		return "refused: the re-check failed"
	case !found:
		return "refused: no longer matches a searched name, or is under an active restriction"
	case m.LinkedHereSource != "":
		return "refused: already linked (" + m.LinkedHereSource + ")"
	}
	added, err := s.resolveLinks.AddLinkIfAbsent(ctx, req.SubjectID, datasubject.Link{
		Table: datasubject.TableProjectMemoryChunks, RowID: m.ChunkID, ProjectID: m.ProjectID,
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceProbable,
		Exclusivity: datasubject.UnknownExclusivity,
	})
	switch {
	case err != nil:
		return "refused: the link could not be written"
	case !added:
		return "refused: already linked"
	}
	return "ok"
}

// recheckChunk re-runs the free-text search for one chunk: it must still match
// a searched term and still be unrestricted (the predicate is in the SQL).
func (s *Server) recheckChunk(ctx context.Context, subjectID, chunkID string, terms []string, project string) (persistence.SubjectTextMention, bool, error) {
	for _, t := range terms {
		got, err := s.resolveText.FindMentions(ctx, persistence.SubjectTextQuery{
			SubjectID: subjectID, Term: t, ProjectID: project, ChunkID: chunkID, Limit: 1,
		})
		if err != nil {
			return persistence.SubjectTextMention{}, false, err
		}
		if len(got) == 1 {
			return got[0], true, nil
		}
	}
	return persistence.SubjectTextMention{}, false, nil
}

func (s *Server) resolveKGCandidates(ctx context.Context, svc *subjectresolve.Service, subjectID string, names, projects []string, data *OperatorSubjectResolveData) {
	// Candidates then the Art 18 screen, shared with the CLI (design §4.17.9).
	// Fail closed: no screen answer, no list.
	cands, err := svc.ScreenedCandidates(ctx, subjectID, names, projects)
	if errors.Is(err, subjectresolve.ErrScreen) {
		s.logger.Warn().Err(subjectresolve.RootCause(err)).Str("request_id", data.RequestID).Str("subject_id", subjectID).
			Msg("subject resolve: restriction screen failed")
		data.KGWithheld = "Knowledge-graph candidates withheld: the processing-restriction check failed, and an unscreened list is never shown."
		return
	}
	if err != nil {
		// Root cause only: the resolver's wrapping quotes the searched name.
		s.logger.Warn().Err(subjectresolve.RootCause(err)).Str("request_id", data.RequestID).Str("subject_id", subjectID).
			Msg("subject resolve: KG candidates failed")
		data.Errors = append(data.Errors, "Knowledge-graph candidates could not be read; the daemon log has the detail.")
		return
	}
	for _, c := range cands {
		row := resolveKGRow{EntityID: c.Entity.ID, ProjectID: c.Entity.ProjectID}
		if c.Withheld {
			row.Withheld = true
			data.KG = append(data.KG, row)
			continue
		}
		row.Name, row.Aliases, row.MatchedOn, row.Mentions = c.Entity.CanonicalName, strings.Join(c.Entity.Aliases, ", "), c.MatchedOn, c.MentionCount
		switch c.State {
		case datasubject.KGBoundHere:
			row.State, row.CanConfirm = "already bound to this subject (confirm tops up new mentions)", true
		case datasubject.KGBoundToPlaceholder:
			row.State, row.CanConfirm, row.Placeholder = "held by placeholder "+c.BoundSubjectID, true, true
		case datasubject.KGConflict:
			row.State = "held by another identified subject " + c.BoundSubjectID + " — reconcile by hand"
		default:
			row.State, row.CanConfirm = "free", true
		}
		data.KG = append(data.KG, row)
	}
	// The resolver allows thousands of candidates per name and project; the
	// page renders at most resolveKGCap (code review 2026-10-12, finding 1).
	if len(data.KG) > resolveKGCap {
		data.KG, data.KGCapped = data.KG[:resolveKGCap], true
	}
}

func (s *Server) resolveTextCandidates(ctx context.Context, subjectID string, terms []string, data *OperatorSubjectResolveData) {
	seen := map[string]bool{}
	for _, t := range terms {
		remaining := resolveTextCap + 1 - len(data.Text)
		if remaining <= 0 {
			break
		}
		got, err := s.resolveText.FindMentions(ctx, persistence.SubjectTextQuery{
			SubjectID: subjectID, Term: t, ProjectID: data.Project, Limit: remaining,
		})
		if err != nil {
			s.logger.Warn().Err(subjectresolve.RootCause(err)).Str("request_id", data.RequestID).Str("subject_id", subjectID).
				Msg("subject resolve: free-text search failed")
			data.Errors = append(data.Errors, "The free-text search could not be run; the daemon log has the detail.")
			return
		}
		for _, m := range got {
			if seen[m.ChunkID] {
				continue
			}
			seen[m.ChunkID] = true
			data.Text = append(data.Text, textRow(m, terms))
		}
	}
	if len(data.Text) > resolveTextCap {
		data.Text, data.TextCapped = data.Text[:resolveTextCap], true
	}
}

func textRow(m persistence.SubjectTextMention, terms []string) resolveTextRow {
	row := resolveTextRow{ChunkID: m.ChunkID, ProjectID: m.ProjectID, Created: m.CreatedAt,
		LinkedSource: m.LinkedHereSource, OtherSubjects: m.OtherSubjects}
	row.CanConfirm = m.LinkedHereSource == ""
	row.CanUnlink = m.LinkedHereSource == string(datasubject.SourceOperatorAsserted)
	pre, match, post, ok := snippet(m.Content, terms, resolveSnippetRunes)
	if !ok {
		// No visible evidence, no confirm: the operator cannot judge a
		// candidate they cannot read (code review round 2, finding 7).
		row.SnippetWithheld, row.CanConfirm = true, false
		return row
	}
	row.Pre, row.Match, row.Post = pre, match, post
	return row
}

// snippet cuts at most width runes either side of the first case-insensitive
// match of any term. When no term can be located (the database folded a
// character Go did not), it reports !ok and the caller withholds the snippet
// rather than showing the start of the record.
func snippet(content string, terms []string, width int) (pre, match, post string, ok bool) {
	runes := []rune(content)
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	at, n := -1, 0
	for _, t := range terms {
		tl := []rune(strings.ToLower(t))
		if i := indexRunes(lower, tl); i >= 0 && (at < 0 || i < at) {
			at, n = i, len(tl)
		}
	}
	if at < 0 {
		return "", "", "", false
	}
	start, end := at-width, at+n+width
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}
	pre, match, post = string(runes[start:at]), string(runes[at:at+n]), string(runes[at+n:end])
	if start > 0 {
		pre = "…" + pre
	}
	if end < len(runes) {
		post += "…"
	}
	return pre, match, post, true
}

func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(hay) {
		return -1
	}
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j, r := range needle {
			if hay[i+j] != r {
				continue outer
			}
		}
		return i
	}
	return -1
}

// resolveCoverage fills the coverage signal: is a KG entity bound, and how many
// operator-asserted chunk links exist (the same test as the CLI's
// warnIfKGUnresolved).
func (s *Server) resolveCoverage(ctx context.Context, subjectID string, data *OperatorSubjectResolveData) {
	if ids, err := s.resolveLinks.ListIdentifiers(ctx, subjectID); err == nil {
		for _, id := range ids {
			if id.Kind == datasubject.KindKGEntity {
				data.KGBound = true
			}
		}
	}
	if links, err := s.resolveLinks.ListLinks(ctx, subjectID); err == nil {
		for _, l := range links {
			if l.Table == datasubject.TableProjectMemoryChunks && l.Source == datasubject.SourceOperatorAsserted {
				data.OperatorLinks++
			}
		}
	}
}

// searchLimiter is a per-principal sliding window. In-process: it resets on
// restart and is per daemon — a bound on a console session, not a security
// boundary (the audit log is the detection surface, design §4.17.2).
type searchLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newSearchLimiter(limit int, window time.Duration) *searchLimiter {
	return &searchLimiter{max: limit, window: window, hits: map[string][]time.Time{}}
}

// allow records and admits one search for key, or refuses it.
func (l *searchLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
