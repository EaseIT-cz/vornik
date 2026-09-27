package ui

// /ui/operator/regulatory — the regulatory record (regulatory record design,
// https://docs.vornik.io). One page,
// in BOTH editions, for every GDPR and EU AI Act action the product records,
// organised by statutory obligation rather than by actor: the two clocks
// (GDPR Art 12(3), Art 33(1)) first, then every subject request, every
// incident, the Art 50 disclosure evidence, and pointers to the trails that
// have their own page.
//
// Gate: the CE operator capability (operatorCapable), never the Enterprise
// admin gate, which answers 501 on Community.
//
// The page is itself processing — its rows concern identified people — so it
// shows ids, grounds, dispositions, deadlines and hashes, never the personal
// data exercised, exported or erased: no subject identifiers beyond the opaque
// subject id, no incident free text, no disclosure session ids.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence"
)

// RegulatoryRequests is the slice of the data-subject ledger the page reads.
// Implemented by *postgres.DataSubjectRepository; nil on SQLite, which has no
// ledger.
type RegulatoryRequests interface {
	ListRequests(ctx context.Context, limit, offset int) ([]datasubject.Request, error)
	ListLiveRequests(ctx context.Context) ([]datasubject.Request, error)
	GetRequestReport(ctx context.Context, id string) (reportJSON, hash string, err error)
	GetRequest(ctx context.Context, id string) (datasubject.Request, error)
}

// RegulatoryIncidents is the slice of the breach ledger the page reads.
// Implemented by *postgres.IncidentRepository; nil on SQLite.
type RegulatoryIncidents interface {
	List(ctx context.Context, limit, offset int) ([]incident.Incident, error)
	ListLive(ctx context.Context) ([]incident.Incident, error)
}

// RegulatoryDisclosures is the Art 50 evidence summary, on both backends.
type RegulatoryDisclosures interface {
	SummaryBetween(ctx context.Context, from, to time.Time) ([]persistence.ChannelDisclosureSummary, error)
}

// WithRegulatoryLedgers wires the regulatory record. Nil requests/incidents
// means "not recorded on this backend"; a nil adminAudit disables the report
// download entirely — never an unaudited one.
func WithRegulatoryLedgers(requests RegulatoryRequests, incidents RegulatoryIncidents,
	disclosures RegulatoryDisclosures, adminAudit persistence.AdminAuditRepository) ServerOption {
	return func(s *Server) {
		s.regRequests, s.regIncidents, s.regDisclosures, s.regAudit = requests, incidents, disclosures, adminAudit
	}
}

const (
	regulatoryPageSize       = 50
	regulatoryDefaultDays    = 30
	regulatoryMaxDays        = 366
	regulatoryHashDisplayLen = 12
)

// requestArticle maps a right to the article a regulator names it by.
var requestArticle = map[datasubject.RequestKind]string{
	datasubject.RequestAccess:        "Art 15",
	datasubject.RequestRectification: "Art 16",
	datasubject.RequestErasure:       "Art 17",
	datasubject.RequestRestriction:   "Art 18",
	datasubject.RequestPortability:   "Art 20",
	datasubject.RequestObjection:     "Art 21",
}

type regRequestRow struct {
	ID, SubjectID, Right, State, Opened, Deadline, Verified string
	Extended, Refused, ErasureGround, HashShort, HashFull   string
	NotRetained                                             string
	Overdue, Attention, HasReport                           bool
}

type regIncidentRow struct {
	ID, State, Aware, Deadline, AuthorityRisk, SubjectRisk string
	NotifiedAuthority, AuthorityRef, NotifiedSubjects      string
	Exemption, Closed                                      string
	FactsLen, EffectsLen, RemedialLen                      int
	Overdue, Attention                                     bool
}

type regDisclosureRow struct {
	Channel, HashShort, HashFull, First, Last string
	Count                                     int
}

// OperatorRegulatoryData backs operator_regulatory.html.
type OperatorRegulatoryData struct {
	adminCommonData
	RequestsRecorded, IncidentsRecorded bool
	AttentionRequests                   []regRequestRow
	AttentionIncidents                  []regIncidentRow
	Requests                            []regRequestRow
	Incidents                           []regIncidentRow
	RequestsNext, IncidentsNext         string
	Disclosures                         []regDisclosureRow
	DisclosureDays                      int
	RangeError                          string
	DownloadsEnabled                    bool
	FirewallAvailable, EnterpriseAdmin  bool
	Errors                              []string
}

func (s *Server) operatorRegulatoryRouter(w http.ResponseWriter, r *http.Request) {
	if !s.operatorCapable(r) {
		http.Error(w, "operator capability required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/operator/regulatory"), "/")
	if rest == "" {
		s.OperatorRegulatory(w, r)
		return
	}
	id, ok := strings.CutPrefix(rest, "requests/")
	if id, isReport := strings.CutSuffix(id, "/report"); ok && isReport && id != "" && !strings.Contains(id, "/") {
		s.operatorRegulatoryReport(w, r, id)
		return
	}
	http.NotFound(w, r)
}

// OperatorRegulatory handles GET /ui/operator/regulatory.
func (s *Server) OperatorRegulatory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	now := time.Now()
	data := OperatorRegulatoryData{
		adminCommonData:   adminCommonData{Title: "Regulatory record", CurrentPage: "regulatory", IsAdmin: true},
		RequestsRecorded:  s.regRequests != nil,
		IncidentsRecorded: s.regIncidents != nil,
		DownloadsEnabled:  s.regRequests != nil && s.regAudit != nil,
		FirewallAvailable: s.memoryPolicyEvaluations != nil,
		EnterpriseAdmin:   s.enterpriseAdmin,
	}
	s.regulatoryRequests(ctx, r, now, &data)
	s.regulatoryIncidents(ctx, r, now, &data)
	s.regulatoryDisclosures(ctx, r, now, &data)
	s.render(w, "operator_regulatory.html", data)
}

func pageOffset(r *http.Request, key string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// regulatoryLedger is the shape both statutory ledgers share for the page: a
// live list (the clock) and a paged full list (the record).
type regulatoryLedger[T any, R any] struct {
	name, param        string
	live               func(context.Context) ([]T, error)
	page               func(ctx context.Context, limit, offset int) ([]T, error)
	attention          func(T) bool
	row                func(T) R
	attentionOut, rows *[]R
	next               *string
}

func (l regulatoryLedger[T, R]) load(ctx context.Context, r *http.Request, errs *[]string, s *Server) {
	if live, err := l.live(ctx); err != nil {
		s.logger.Warn().Err(err).Str("ledger", l.name).Msg("regulatory record: live list failed")
		*errs = append(*errs, "Live "+l.name+" could not be read; the daemon log has the detail.")
	} else {
		for _, item := range live {
			if l.attention(item) {
				*l.attentionOut = append(*l.attentionOut, l.row(item))
			}
		}
	}
	offset := pageOffset(r, l.param)
	items, err := l.page(ctx, regulatoryPageSize+1, offset)
	if err != nil {
		s.logger.Warn().Err(err).Str("ledger", l.name).Msg("regulatory record: list failed")
		*errs = append(*errs, strings.ToUpper(l.name[:1])+l.name[1:]+" could not be read; the daemon log has the detail.")
		return
	}
	if len(items) > regulatoryPageSize {
		items = items[:regulatoryPageSize]
		*l.next = fmt.Sprintf("?%s=%d", l.param, offset+regulatoryPageSize)
	}
	for _, item := range items {
		*l.rows = append(*l.rows, l.row(item))
	}
}

func (s *Server) regulatoryRequests(ctx context.Context, r *http.Request, now time.Time, data *OperatorRegulatoryData) {
	if s.regRequests == nil {
		return
	}
	regulatoryLedger[datasubject.Request, regRequestRow]{
		name: "subject requests", param: "req",
		live: s.regRequests.ListLiveRequests, page: s.regRequests.ListRequests,
		attention:    func(q datasubject.Request) bool { return q.NeedsAttention(now) },
		row:          func(q datasubject.Request) regRequestRow { return requestRow(q, now) },
		attentionOut: &data.AttentionRequests, rows: &data.Requests, next: &data.RequestsNext,
	}.load(ctx, r, &data.Errors, s)
}

func (s *Server) regulatoryIncidents(ctx context.Context, r *http.Request, now time.Time, data *OperatorRegulatoryData) {
	if s.regIncidents == nil {
		return
	}
	regulatoryLedger[incident.Incident, regIncidentRow]{
		name: "incidents", param: "inc",
		live: s.regIncidents.ListLive, page: s.regIncidents.List,
		attention:    func(i incident.Incident) bool { return i.NeedsAttention(now) },
		row:          func(i incident.Incident) regIncidentRow { return incidentRow(i, now) },
		attentionOut: &data.AttentionIncidents, rows: &data.Incidents, next: &data.IncidentsNext,
	}.load(ctx, r, &data.Errors, s)
}

func (s *Server) regulatoryDisclosures(ctx context.Context, r *http.Request, now time.Time, data *OperatorRegulatoryData) {
	data.DisclosureDays = regulatoryDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > regulatoryMaxDays {
			data.RangeError = fmt.Sprintf("The disclosure range must be between 1 and %d days; showing the last %d.",
				regulatoryMaxDays, regulatoryDefaultDays)
		} else {
			data.DisclosureDays = n
		}
	}
	if s.regDisclosures == nil {
		return
	}
	rows, err := s.regDisclosures.SummaryBetween(ctx, now.Add(-time.Duration(data.DisclosureDays)*24*time.Hour), now)
	if err != nil {
		s.logger.Warn().Err(err).Msg("regulatory record: disclosure summary failed")
		data.Errors = append(data.Errors, "AI-disclosure evidence could not be read; the daemon log has the detail.")
		return
	}
	for _, d := range rows {
		data.Disclosures = append(data.Disclosures, regDisclosureRow{
			Channel: d.Channel, Count: d.Count, HashFull: d.TextHash, HashShort: shortHashText(d.TextHash),
			First: stamp(d.FirstServed), Last: stamp(d.LastServed),
		})
	}
}

// operatorRegulatoryReport serves a retained report, audited FIRST (design §4,
// review F3/F4): no audit sink → refuse before reading; the admin_audit row is
// written before any header or byte of the report; a failed write refuses with
// no attachment headers and no body.
func (s *Server) operatorRegulatoryReport(w http.ResponseWriter, r *http.Request, id string) {
	if s.regRequests == nil {
		http.Error(w, "not recorded on this deployment: the SQLite backend has no subject-request ledger", http.StatusNotFound)
		return
	}
	if s.regAudit == nil {
		http.Error(w, "report downloads are unavailable: no audit log is wired, and an unaudited download is refused", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	body, hash, err := s.regRequests.GetRequestReport(ctx, id)
	switch {
	case errors.Is(err, persistence.ErrReportNotRetained):
		// Per right (design §4.1, review F2): only an access or portability
		// request is an export, so only those are told the export reason.
		reason := notRetainedReason("")
		if req, gerr := s.regRequests.GetRequest(ctx, id); gerr == nil {
			reason = notRetainedReason(req.Kind)
		}
		http.Error(w, "no retained report for this request: "+reason, http.StatusNotFound)
		return
	case errors.Is(err, persistence.ErrNotFound):
		http.Error(w, "no such request", http.StatusNotFound)
		return
	case err != nil:
		s.logger.Warn().Err(err).Str("request_id", id).Msg("regulatory record: report read failed")
		http.Error(w, "the report could not be read", http.StatusInternalServerError)
		return
	}
	after, _ := json.Marshal(map[string]string{"request_id": id, "report_hash": hash})
	if err := s.regAudit.Insert(ctx, &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Timestamp: time.Now().UTC(),
		Principal: adminPrincipal(r),
		Source:    "ui",
		Action:    "regulatory.report.download",
		Target:    id,
		After:     string(after),
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	}); err != nil {
		s.logger.Warn().Err(err).Str("request_id", id).Msg("regulatory record: report download refused — audit write failed")
		http.Error(w, "the download could not be recorded, so it was refused", http.StatusInternalServerError)
		return
	}
	s.logger.Info().Str("request_id", id).Str("report_hash", hash).Str("principal", adminPrincipal(r)).
		Msg("regulatory record: subject report downloaded")
	// Headers only now, AFTER the audit row is written (design §4, F4): nothing
	// about the report reaches the response before its download is on record.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="subject-request-%s-report.json"`, safeFilenamePart(id)))
	_, _ = w.Write([]byte(body))
}

// notRetainedReason says why a request has a hash and no report. Exports are
// not retained (operator decision 2026-09-25, design §4.1): the export IS the
// subject's personal data. Any other right says only that it was not retained,
// never that it was an export. One function for the page and the 404, so the
// two cannot disagree.
func notRetainedReason(kind datasubject.RequestKind) string {
	if kind == datasubject.RequestAccess || kind == datasubject.RequestPortability {
		return "not retained: the export is the subject's personal data, so only its hash is kept. " +
			"The hash identifies the copy that was sent; it cannot reproduce it"
	}
	return "not retained"
}

func requestRow(req datasubject.Request, now time.Time) regRequestRow {
	right := string(req.Kind)
	if art, ok := requestArticle[req.Kind]; ok {
		right = art + " " + right
	}
	row := regRequestRow{
		ID: req.ID, SubjectID: req.SubjectID, Right: right,
		State: string(req.State), Opened: stamp(req.OpenedAt), Deadline: stamp(req.Deadline()),
		Refused: req.RefusedReason, ErasureGround: string(req.ErasureGround),
		HashFull: req.ReportHash, HashShort: shortHashText(req.ReportHash),
		Overdue: req.Overdue(now), Attention: req.NeedsAttention(now),
		HasReport: req.ReportRetained,
	}
	// Exports are not retained (operator decision 2026-09-25, design §4.1): a
	// hash with no report is said out loud, never left as a blank cell.
	if req.ReportHash != "" && !req.ReportRetained {
		row.NotRetained = notRetainedReason(req.Kind)
	}
	if !req.VerifiedAt.IsZero() {
		row.Verified = stamp(req.VerifiedAt) + " (" + req.VerifiedHow + ")"
	}
	if req.Extended {
		row.Extended = "extended: " + req.ExtendedReason
	}
	return row
}

func incidentRow(i incident.Incident, now time.Time) regIncidentRow {
	return regIncidentRow{
		ID: i.ID, State: string(i.State), Aware: stamp(i.BecameAwareAt), Deadline: stamp(i.Deadline()),
		AuthorityRisk: riskText(i.AuthorityRisk, i.AuthorityRiskReason != ""), SubjectRisk: riskText(i.SubjectRisk, i.SubjectRiskReason != ""),
		NotifiedAuthority: stamp(i.NotifiedAuthorityAt), AuthorityRef: i.AuthorityReference,
		NotifiedSubjects: stamp(i.NotifiedSubjectsAt), Exemption: i.SubjectExemption, Closed: stamp(i.ClosedAt),
		// Lengths only: the Art 33(5) text is operator-written and can name people.
		FactsLen: len(i.Facts), EffectsLen: len(i.Effects), RemedialLen: len(i.Remedial),
		Overdue: i.Overdue(now), Attention: i.NeedsAttention(now),
	}
}

func riskText(answer string, reasoned bool) string {
	switch {
	case answer == "":
		return "not assessed"
	case reasoned:
		return answer + " (reason recorded)"
	default:
		return answer
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func shortHashText(h string) string {
	if len(h) > regulatoryHashDisplayLen {
		return h[:regulatoryHashDisplayLen]
	}
	return h
}

// safeFilenamePart keeps a request id usable in a Content-Disposition filename.
func safeFilenamePart(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, id)
}
