package approverdevice

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

//go:embed templates/*.html
var templateFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"page": func(title string, refresh int) map[string]any {
		return map[string]any{"Title": title, "Refresh": refresh}
	},
	"when": func(t time.Time) string { return t.Format("2 Jan 2006 15:04 MST") },
	"whenp": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Format("2 Jan 2006 15:04 MST")
	},
}).ParseFS(templateFS, "templates/*.html"))

// Route is one path this package serves. The refusal matrix (plan P2.7)
// iterates Routes, so a route added here is covered without being copied
// into the test.
type Route struct {
	Method, Path string
	DeviceOnly   bool
}

// Routes lists every route, with a concrete example path for patterns.
func Routes() []Route {
	return []Route{
		{http.MethodGet, "/ui/pair", false},
		{http.MethodPost, "/ui/pair", false},
		{http.MethodGet, "/ui/pair/wait", false},
		{http.MethodGet, "/ui/approve/", true},
		{http.MethodGet, "/ui/approve/apr_example", true},
		{http.MethodPost, "/ui/approve/apr_example", true},
		{http.MethodGet, "/ui/approve/devices", true},
		{http.MethodPost, "/ui/approve/devices/dev_example/revoke", true},
		{http.MethodPost, "/ui/approve/apr_example/connect", true},
		{http.MethodGet, "/ui/approve/standing", true},
		{http.MethodPost, "/ui/approve/standing/bsg_example/revoke", true},
	}
}

// MountPrefixes are the prefixes the daemon mounts on its OUTER mux, ahead of
// /ui/, so these routes never pass AuthMiddleware (plan "Mounting decision").
var MountPrefixes = []string{"/ui/pair", "/ui/pair/", "/ui/approve/"}

// Handler serves the pairing and approval pages. wrap is applied outermost
// (the daemon passes its per-IP limiter, plan amendment 2); nil means none.
func (s *Service) Handler(wrap func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ui/pair", s.pairPage)
	mux.HandleFunc("/ui/pair/wait", s.pairWait)
	mux.Handle("/ui/approve/", s.RequireDevice(http.HandlerFunc(s.approveRouter)))
	h := securityHeaders(mux)
	if wrap != nil {
		h = wrap(h)
	}
	return h
}

// pageCSP keeps the approval pages same-origin and script-free (design
// §9.1a): credentials are typed here, so nothing from another origin may run
// or load. Style is inline, hence 'unsafe-inline' for style only.
const pageCSP = "default-src 'self'; script-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// securityHeaders sets the CSP on every response of these routes, before any
// handler runs, so redirects and errors carry it too (review e076 F4).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pageCSP)
		next.ServeHTTP(w, r)
	})
}

// notifyLine says how this device hears of new requests (§9.1a).
func (s *Service) notifyLine() string {
	name := strings.ToUpper(s.notifyChannel[:min(1, len(s.notifyChannel))]) + s.notifyChannel[min(1, len(s.notifyChannel)):]
	switch {
	case s.notifyChannel != "" && s.notifyActive && s.notify != nil:
		return "New requests are announced on " + name + ". Vornik sends no browser notifications."
	case s.notifyChannel != "":
		return name + " is configured but switched off, so new requests are not announced. Open this page to check."
	}
	return "No notification channel is configured, so new requests are not announced. Open this page to check."
}

func clientIP(r *http.Request) string {
	if ip := realip.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return realip.RemoteHost(r)
}

func (s *Service) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "page error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type statusPage struct{ Title, Message string }

func (s *Service) renderStatus(w http.ResponseWriter, status int, title, msg string) {
	s.render(w, status, "status.html", statusPage{title, msg})
}

// staleMessage answers a decision whose device value moved on while the page
// was open (a value neither current nor shared).
const staleMessage = "This device's sign-in changed while this page was open. Open the approvals list again; if it asks you to pair, follow the steps there."

// pairData backs pair.html and wait.html.
type pairData struct {
	Error string
	Help  pairingHelp
	// SignedOut explains why this browser's device value is dead, and Resume
	// says a code restores the same device (design §9.2, amendment
	// 2026-10-05). Both come only from the dead cookie this browser
	// presented, never from the query.
	SignedOut string
	Resume    bool
}

// signedOut reads the device cookie on the pairing page: a valid device is
// sent on to next; a recognised dead value is explained, and an expired one
// is resumable.
func (s *Service) signedOut(w http.ResponseWriter, r *http.Request, next string) (data pairData, done bool) {
	data = pairData{Help: s.help(r, next)}
	c, err := r.Cookie(CookieName)
	if err != nil {
		return data, false
	}
	res, err := s.authenticate(r.Context(), c.Value)
	var dead *ErrDeadDevice
	switch {
	case err == nil && res.device != nil:
		if res.reissue != "" {
			w = s.withCookie(w, r, res.reissue)
		}
		http.Redirect(w, r, orDefault(next), http.StatusSeeOther)
		return data, true
	case errors.As(err, &dead) && dead.Reason == persistence.DeadExpired:
		data.SignedOut = "This phone was signed out because the answer to its last approval did not reach it. Enter a new code from vornikctl pair-device to restore it."
		data.Resume = true
	case errors.As(err, &dead):
		data.SignedOut = "This phone was signed out because another browser used its sign-in. If that was not you, revoke this device from another device or with vornikctl devices revoke. To pair this browser again, enter a code; a device you already use must approve it."
	}
	return data, false
}

// pairPage is the only unauthenticated POST: code entry, rate limited in the
// service (per IP and globally) and same-origin checked here.
func (s *Service) pairPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		data, done := s.signedOut(w, r, validNext(r.URL.Query().Get("next")))
		if done {
			return
		}
		s.render(w, http.StatusOK, "pair.html", data)
	case http.MethodPost:
		if err := approval.CheckRequest(r); err != nil {
			s.renderStatus(w, http.StatusForbidden, "Refused", "This request did not come from this page.")
			return
		}
		next := validNext(r.FormValue("next"))
		data, done := s.signedOut(w, r, next)
		if done {
			return
		}
		if data.Resume {
			// An expired dead value plus a terminal code restores the SAME
			// device (design §9.2, amendment 2026-10-05).
			c, _ := r.Cookie(CookieName)
			tok, _, err := s.Resume(r.Context(), r.FormValue("code"), c.Value, clientIP(r))
			switch {
			case errors.Is(err, ErrRateLimited):
				data.Error = err.Error()
				s.render(w, http.StatusTooManyRequests, "pair.html", data)
			case errors.Is(err, ErrBadCode):
				data.Error = err.Error()
				s.render(w, http.StatusBadRequest, "pair.html", data)
			case err != nil:
				s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			default:
				SetCookie(w, r, tok)
				http.Redirect(w, r, orDefault(next), http.StatusSeeOther)
			}
			return
		}
		res, err := s.Redeem(r.Context(), r.FormValue("code"), clientIP(r))
		switch {
		case errors.Is(err, ErrRateLimited):
			data.Error = err.Error()
			s.render(w, http.StatusTooManyRequests, "pair.html", data)
			return
		case errors.Is(err, ErrBadCode):
			data.Error = err.Error()
			s.render(w, http.StatusBadRequest, "pair.html", data)
			return
		case err != nil:
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		if res.DeviceToken != "" {
			SetCookie(w, r, res.DeviceToken)
			http.Redirect(w, r, orDefault(next), http.StatusSeeOther)
			return
		}
		setClaimCookie(w, r, res.ClaimToken)
		if next != "" {
			setNextCookie(w, r, next)
		} else {
			clearNextCookie(w, r)
		}
		http.Redirect(w, r, "/ui/pair/wait", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) pairWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie(ClaimCookieName)
	if err != nil {
		// The second tab, after the first completed (plan amendment 9).
		if dc, derr := r.Cookie(CookieName); derr == nil {
			if res, aerr := s.authenticate(r.Context(), dc.Value); aerr == nil && res.device != nil {
				if res.reissue != "" {
					w = s.withCookie(w, r, res.reissue)
				}
				next := nextFromCookie(r)
				clearNextCookie(w, r)
				http.Redirect(w, r, orDefault(next), http.StatusSeeOther)
				return
			}
		}
		http.Redirect(w, r, "/ui/pair", http.StatusSeeOther)
		return
	}
	tok, st, err := s.PollClaim(r.Context(), c.Value)
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	switch st {
	case ClaimApproved:
		next := nextFromCookie(r)
		clearClaimCookie(w, r)
		clearNextCookie(w, r)
		SetCookie(w, r, tok)
		http.Redirect(w, r, orDefault(next), http.StatusSeeOther)
	case ClaimPending:
		s.render(w, http.StatusOK, "wait.html", pairData{Help: s.help(r, nextFromCookie(r))})
	case ClaimRejected:
		clearClaimCookie(w, r)
		clearNextCookie(w, r)
		s.renderStatus(w, http.StatusOK, "Not approved", "This device was not approved.")
	default:
		clearClaimCookie(w, r)
		clearNextCookie(w, r)
		s.renderStatus(w, http.StatusOK, "Expired", "This pairing expired. Start again with vornikctl pair-device.")
	}
}

func (s *Service) approveRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ui/approve/")
	switch {
	case rest == "":
		s.listPage(w, r)
	case rest == "devices":
		s.devicesPage(w, r)
	case rest == "standing" || strings.HasPrefix(rest, "standing/"):
		s.standingPage(w, r, strings.TrimPrefix(strings.TrimPrefix(rest, "standing"), "/"))
	case strings.HasPrefix(rest, "devices/") && strings.HasSuffix(rest, "/revoke"):
		s.revoke(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "devices/"), "/revoke"))
	case strings.HasSuffix(rest, "/connect") && !strings.Contains(strings.TrimSuffix(rest, "/connect"), "/"):
		s.connect(w, r, strings.TrimSuffix(rest, "/connect"))
	case strings.HasPrefix(rest, "group/") && !strings.Contains(strings.TrimPrefix(rest, "group/"), "/"):
		s.groupPage(w, r, strings.TrimPrefix(rest, "group/"))
	case !strings.Contains(rest, "/"):
		s.requestPage(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

type listData struct {
	Device  *Device
	Pending []persistence.AgentApprovalRequestRow
	Groups  []listGroup
	Flash   string
	Refused []string
	Notify  string
}

// listGroup is one group of two or more requests reviewed together.
type listGroup struct {
	Key, Title string
	Count      int
}

func (s *Service) listPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	pend, err := s.ListPending(r.Context())
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	singles, groups := s.groupPending(pend)
	var refused []string
	if v := r.URL.Query().Get("refused"); v != "" {
		refused = strings.Split(v, ",")
	}
	s.render(w, http.StatusOK, "list.html", listData{Device: d, Pending: singles, Groups: groups,
		Flash: r.URL.Query().Get("done"), Refused: refused, Notify: s.notifyLine()})
}

// groupPending splits pending requests into groups of two or more (by their
// describer's Group) and the rest, listed one by one as before.
func (s *Service) groupPending(pend []persistence.AgentApprovalRequestRow) ([]persistence.AgentApprovalRequestRow, []listGroup) {
	members := map[string][]persistence.AgentApprovalRequestRow{}
	titles := map[string]string{}
	var order []string
	for _, r := range pend {
		if d := s.describe(r); d != nil && validGroupKey(d.Group) && s.batchable(r.Kind) {
			if _, seen := members[d.Group]; !seen {
				order = append(order, d.Group)
			}
			members[d.Group] = append(members[d.Group], r)
			titles[d.Group] = d.GroupTitle
		}
	}
	grouped := map[string]bool{}
	var groups []listGroup
	for _, k := range order {
		if len(members[k]) < 2 {
			continue
		}
		groups = append(groups, listGroup{Key: k, Title: titles[k], Count: len(members[k])})
		for _, r := range members[k] {
			grouped[r.ID] = true
		}
	}
	var singles []persistence.AgentApprovalRequestRow
	for _, r := range pend {
		if !grouped[r.ID] {
			singles = append(singles, r)
		}
	}
	return singles, groups
}

func validGroupKey(k string) bool {
	if k == "" || len(k) > 120 {
		return false
	}
	for _, c := range k {
		lower, digit := c >= 'a' && c <= 'z', c >= '0' && c <= '9'
		if !lower && !digit && c != '_' {
			return false
		}
	}
	return true
}

// batchable: a kind decided by a plain approve or reject. A value entry or
// a sign-in is never decided in a batch.
func (s *Service) batchable(kind string) bool {
	if _, takesValue := s.valueEntry(kind); takesValue {
		return false
	}
	s.mu.RLock()
	_, connect := s.connects[kind]
	s.mu.RUnlock()
	return !connect
}

type groupData struct {
	Key, Title string
	Items      []requestData
	Notice     string
}

// groupPage shows a group's requests together (GET) and decides the picked
// ones (POST), each against its own shown hash.
func (s *Service) groupPage(w http.ResponseWriter, r *http.Request, key string) {
	if !validGroupKey(key) {
		http.NotFound(w, r)
		return
	}
	pend, err := s.ListPending(r.Context())
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	var items []requestData
	title := ""
	inGroup := map[string]*persistence.AgentApprovalRequestRow{}
	for i := range pend {
		req := &pend[i]
		if d := s.describe(*req); d != nil && d.Group == key && s.batchable(req.Kind) {
			items = append(items, s.requestData(req, ""))
			inGroup[req.ID] = req
			title = d.GroupTitle
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if len(items) == 0 {
			http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
			return
		}
		s.render(w, http.StatusOK, "group.html", groupData{Key: key, Title: title, Items: items})
	case http.MethodPost:
		s.decideGroup(w, r, key, inGroup)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) decideGroup(w http.ResponseWriter, r *http.Request, key string, inGroup map[string]*persistence.AgentApprovalRequestRow) {
	d, _ := FromRequest(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	if !approve && r.PostFormValue("decision") != "reject" {
		http.Redirect(w, r, "/ui/approve/group/"+key, http.StatusSeeOther)
		return
	}
	picks := r.PostForm["pick"]
	if len(picks) == 0 {
		http.Redirect(w, r, "/ui/approve/group/"+key, http.StatusSeeOther)
		return
	}
	rot, err := s.rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Signed in elsewhere", staleMessage)
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// Share the rotation with requests already in flight on this value until
	// ShareGrace after this response; the cookie is written lazily (design
	// §9.2, amendment 2026-10-05).
	defer s.closeShare(r.Context(), d.ID, rot)
	w = s.withCookie(w, r, rot.token)
	decided := 0
	var refused []string
	for _, p := range picks {
		id, shown, ok := strings.Cut(p, "|")
		if !ok || inGroup[id] == nil {
			continue // not in this group (any more): never decided from here
		}
		if err := s.Decide(r.Context(), d, id, shown, approve); err != nil && (errors.Is(err, ErrNotDecidable) || errors.Is(err, ErrUnknownKind) || !approve) {
			refused = append(refused, id)
			continue
		}
		decided++
	}
	verdict := "rejected"
	if approve {
		verdict = "approved"
	}
	loc := "/ui/approve/?done=" + url.QueryEscape(strconv.Itoa(decided)+" "+verdict)
	if len(refused) > 0 {
		loc += "&refused=" + url.QueryEscape(strings.Join(refused, ","))
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

type requestData struct {
	Req *persistence.AgentApprovalRequestRow
	// TakesValue shows a value field: entering the value is the approval.
	TakesValue bool
	// Connect shows a sign-in button instead (an OAuth credential slot).
	Connect  bool
	Pretty   string
	Pending  bool
	Expired  bool
	Notice   string
	Decision string
	// Desc is the plain summary and risk level, when the kind has a
	// describer (§18.7).
	Desc *Description
	// Host is a host_action's page: its choices instead of approve and
	// reject (Hermes approval transport design §4.2).
	Host *hostPage
	// Grant is the standing grant offered under Approve (broker
	// write-actions design, tier 2), or nil.
	Grant *GrantOffer
}

func (s *Service) requestPage(w http.ResponseWriter, r *http.Request, id string) {
	req, err := s.Request(r.Context(), id)
	if errors.Is(err, persistence.ErrNotFound) {
		s.renderStatus(w, http.StatusNotFound, "Not found", "There is no such request.")
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.render(w, http.StatusOK, "request.html", s.requestDataCtx(r.Context(), req, ""))
	case http.MethodPost:
		s.decide(w, r, req)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) requestData(req *persistence.AgentApprovalRequestRow, notice string) requestData {
	return s.requestDataCtx(context.Background(), req, notice)
}

func (s *Service) requestDataCtx(ctx context.Context, req *persistence.AgentApprovalRequestRow, notice string) requestData {
	var pretty bytes.Buffer
	if json.Indent(&pretty, req.Rendered, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(req.Rendered)
	}
	now := s.now()
	_, connect := s.connectEntry(*req)
	_, takesValue := s.valueEntry(req.Kind)
	takesValue = takesValue && !connect
	var host *hostPage
	if req.Kind == persistence.ApprovalKindHostAction {
		host = s.hostPageData(*req)
	}
	d := requestData{Req: req, Pretty: pretty.String(), Notice: notice, TakesValue: takesValue, Connect: connect, Desc: s.describe(*req), Host: host,
		Pending: req.Status == persistence.ApprovalPending && now.Before(req.ExpiresAt),
		Expired: req.Status == persistence.ApprovalExpired || (req.Status == persistence.ApprovalPending && !now.Before(req.ExpiresAt))}
	if d.Pending && !takesValue && !connect && host == nil {
		d.Grant = s.grantOffer(ctx, *req)
	}
	return d
}

func (s *Service) decide(w http.ResponseWriter, r *http.Request, req *persistence.AgentApprovalRequestRow) {
	d, _ := FromRequest(r)
	// A form is small; a value is at most MaxValueBytes (plan P4.2).
	r.Body = http.MaxBytesReader(w, r.Body, MaxValueBytes+4<<10)
	if err := r.ParseForm(); err != nil {
		s.render(w, http.StatusRequestEntityTooLarge, "request.html", s.requestData(req, "That was too long."))
		return
	}
	if req.Kind == persistence.ApprovalKindHostAction {
		s.decideHost(w, r, req)
		return
	}
	if r.PostFormValue("decision") == "approve_grant" {
		s.decideGrant(w, r, req)
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	if !approve && r.PostFormValue("decision") != "reject" {
		s.render(w, http.StatusBadRequest, "request.html", s.requestData(req, "Choose Approve or Reject."))
		return
	}
	entry, takesValue := s.valueEntry(req.Kind)
	if _, connect := s.connectEntry(*req); connect {
		// A sign-in request is approved only by its sign-in coming back.
		if approve {
			s.render(w, http.StatusConflict, "request.html", s.requestData(req, "Use Connect to approve this request."))
			return
		}
		takesValue = false
	}
	value := r.PostFormValue("value")
	if approve && takesValue {
		if value == "" {
			s.render(w, http.StatusBadRequest, "request.html", s.requestData(req, "Enter the value, then approve."))
			return
		}
		if len(value) > MaxValueBytes {
			s.render(w, http.StatusRequestEntityTooLarge, "request.html", s.requestData(req, "That value is too long."))
			return
		}
	}
	// Rotate first (design §9.2): a value neither current nor shared (it moved
	// on while the page was open) is a 409 and decides nothing.
	rot, err := s.rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Signed in elsewhere", staleMessage)
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// Share the rotation with requests already in flight on this value until
	// ShareGrace after this response; the cookie is written lazily (design
	// §9.2, amendment 2026-10-05).
	defer s.closeShare(r.Context(), d.ID, rot)
	w = s.withCookie(w, r, rot.token)
	shown := r.PostFormValue("rendered_sha256")
	if approve && takesValue {
		err = entry(r.Context(), d, *req, shown, []byte(value))
		if errors.Is(err, ErrValueNotStored) {
			s.renderStatus(w, http.StatusInternalServerError, "Not stored",
				"You approved this, but the value could not be stored. Ask your assistant to request it again.")
			return
		}
	} else {
		err = s.Decide(r.Context(), d, req.ID, shown, approve)
	}
	switch {
	case errors.Is(err, ErrNotDecidable):
		fresh, gerr := s.Request(r.Context(), req.ID)
		if gerr != nil {
			fresh = req
		}
		s.render(w, http.StatusConflict, "request.html", s.requestData(fresh, "This request changed after you opened it, or it was already decided. Review it again."))
		return
	case errors.Is(err, ErrUnknownKind):
		s.render(w, http.StatusConflict, "request.html", s.requestData(req, "This kind of request cannot be approved by this version of Vornik yet."))
		return
	case err != nil && (!approve || takesValue):
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	verdict := "rejected"
	if approve {
		verdict = "approved"
	}
	// An approval whose effect failed is still approved; the effect is retried.
	http.Redirect(w, r, "/ui/approve/?done="+verdict, http.StatusSeeOther)
}

// decideHost answers a host_action with one of the choices its page offers
// (Hermes approval transport design §4.2). A choice the page does not offer
// (always, a plain approve) is refused before the token rotates.
func (s *Service) decideHost(w http.ResponseWriter, r *http.Request, req *persistence.AgentApprovalRequestRow) {
	d, _ := FromRequest(r)
	choice := r.PostFormValue("decision")
	offered := false
	for _, c := range HostChoices(*req) {
		offered = offered || c == choice
	}
	if !offered {
		s.render(w, http.StatusBadRequest, "request.html", s.requestData(req, "Choose one of the answers below."))
		return
	}
	rot, err := s.rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Signed in elsewhere", staleMessage)
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// Share the rotation with requests already in flight on this value until
	// ShareGrace after this response; the cookie is written lazily (design
	// §9.2, amendment 2026-10-05).
	defer s.closeShare(r.Context(), d.ID, rot)
	w = s.withCookie(w, r, rot.token)
	err = s.DecideChoice(r.Context(), d, req.ID, r.PostFormValue("rendered_sha256"), choice)
	switch {
	case errors.Is(err, ErrNotDecidable):
		fresh, gerr := s.Request(r.Context(), req.ID)
		if gerr != nil {
			fresh = req
		}
		s.render(w, http.StatusConflict, "request.html", s.requestData(fresh, "This request changed after you opened it, or it was already decided or expired. Review it again."))
		return
	case errors.Is(err, ErrBadChoice):
		s.render(w, http.StatusBadRequest, "request.html", s.requestData(req, "Choose one of the answers below."))
		return
	case err != nil && choice == ChoiceDeny:
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// An approval is recorded even if marking it applied failed; the tick
	// retries the no-op effect.
	verdict := "allowed"
	if choice == ChoiceDeny {
		verdict = "denied"
	}
	http.Redirect(w, r, "/ui/approve/?done="+verdict, http.StatusSeeOther)
}

// decideGrant approves with a standing grant (broker write-actions design,
// tier 2 item 2). A days/uses pair the offer does not hold is refused before
// the token rotates.
func (s *Service) decideGrant(w http.ResponseWriter, r *http.Request, req *persistence.AgentApprovalRequestRow) {
	d, _ := FromRequest(r)
	days, err1 := strconv.Atoi(r.PostFormValue("grant_days"))
	uses, err2 := strconv.Atoi(r.PostFormValue("grant_uses"))
	o := s.grantOffer(r.Context(), *req)
	if err1 != nil || err2 != nil || o == nil || !offered(o.Days, days) || !offered(o.Uses, uses) {
		s.render(w, http.StatusBadRequest, "request.html", s.requestDataCtx(r.Context(), req, "Choose how long and how many times, from the choices below."))
		return
	}
	rot, err := s.rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Signed in elsewhere", staleMessage)
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	// Share the rotation with requests already in flight on this value until
	// ShareGrace after this response; the cookie is written lazily (design
	// §9.2, amendment 2026-10-05).
	defer s.closeShare(r.Context(), d.ID, rot)
	w = s.withCookie(w, r, rot.token)
	err = s.DecideGrant(r.Context(), d, req.ID, r.PostFormValue("rendered_sha256"), days, uses)
	switch {
	case errors.Is(err, ErrNotDecidable):
		fresh, gerr := s.Request(r.Context(), req.ID)
		if gerr != nil {
			fresh = req
		}
		s.render(w, http.StatusConflict, "request.html", s.requestDataCtx(r.Context(), fresh, "This request changed after you opened it, or it was already decided. Review it again."))
		return
	case errors.Is(err, ErrBadChoice):
		s.render(w, http.StatusBadRequest, "request.html", s.requestDataCtx(r.Context(), req, "Choose how long and how many times, from the choices below."))
		return
	case errors.Is(err, ErrUnknownKind):
		s.render(w, http.StatusConflict, "request.html", s.requestDataCtx(r.Context(), req, "This kind of request cannot be approved by this version of Vornik yet."))
		return
	}
	// An approval whose effect failed is still approved; the effect is retried.
	http.Redirect(w, r, "/ui/approve/?done=approved", http.StatusSeeOther)
}

type devicesData struct {
	Current *Device
	Devices []persistence.ApproverDeviceRow
}

func (s *Service) devicesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	devs, err := s.ListDevices(r.Context())
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	s.render(w, http.StatusOK, "devices.html", devicesData{Current: d, Devices: devs})
}

// revoke is not a decision and is not rotated (plan amendment 9).
func (s *Service) revoke(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, _ := FromRequest(r)
	if err := s.Revoke(r.Context(), id); err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	if d != nil && d.ID == id {
		ClearCookie(w, r)
		s.renderStatus(w, http.StatusOK, "Revoked", "This device is no longer an approver.")
		return
	}
	http.Redirect(w, r, "/ui/approve/devices", http.StatusSeeOther)
}

// connect starts a request's sign-in (plan P4.4): device and same origin,
// like every decision, but it decides nothing.
func (s *Service) connect(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := s.Request(r.Context(), id)
	if err != nil {
		s.renderStatus(w, http.StatusNotFound, "Not found", "There is no such request.")
		return
	}
	entry, ok := s.connectEntry(*req)
	data := s.requestData(req, "")
	if !ok || !data.Pending {
		s.render(w, http.StatusConflict, "request.html", s.requestData(req, "This request cannot be connected now."))
		return
	}
	d, _ := FromRequest(r)
	authURL, flow, err := entry.Start(r.Context(), d, *req, r.PostFormValue("rendered_sha256"))
	if errors.Is(err, ErrNotDecidable) {
		s.render(w, http.StatusConflict, "request.html", s.requestData(req, "This request changed after you opened it. Review it again."))
		return
	}
	if err != nil || authURL == "" {
		s.render(w, http.StatusBadGateway, "request.html", s.requestData(req, "The sign-in could not be started. Try again in a moment."))
		return
	}
	if flow != nil {
		http.SetCookie(w, flow)
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}
