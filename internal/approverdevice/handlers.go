package approverdevice

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
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
	var h http.Handler = mux
	if wrap != nil {
		h = wrap(h)
	}
	return h
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

// pairPage is the only unauthenticated POST: code entry, rate limited in the
// service (per IP and globally) and same-origin checked here.
func (s *Service) pairPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.render(w, http.StatusOK, "pair.html", map[string]string{})
	case http.MethodPost:
		if err := approval.CheckRequest(r); err != nil {
			s.renderStatus(w, http.StatusForbidden, "Refused", "This request did not come from this page.")
			return
		}
		res, err := s.Redeem(r.Context(), r.FormValue("code"), clientIP(r))
		switch {
		case errors.Is(err, ErrRateLimited):
			s.render(w, http.StatusTooManyRequests, "pair.html", map[string]string{"Error": err.Error()})
			return
		case errors.Is(err, ErrBadCode):
			s.render(w, http.StatusBadRequest, "pair.html", map[string]string{"Error": err.Error()})
			return
		case err != nil:
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		if res.DeviceToken != "" {
			SetCookie(w, r, res.DeviceToken)
			http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
			return
		}
		setClaimCookie(w, r, res.ClaimToken)
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
			if d, aerr := s.Authenticate(r.Context(), dc.Value); aerr == nil && d != nil {
				http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
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
		clearClaimCookie(w, r)
		SetCookie(w, r, tok)
		http.Redirect(w, r, "/ui/approve/", http.StatusSeeOther)
	case ClaimPending:
		s.render(w, http.StatusOK, "wait.html", nil)
	case ClaimRejected:
		clearClaimCookie(w, r)
		s.renderStatus(w, http.StatusOK, "Not approved", "This device was not approved.")
	default:
		clearClaimCookie(w, r)
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
	case strings.HasPrefix(rest, "devices/") && strings.HasSuffix(rest, "/revoke"):
		s.revoke(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "devices/"), "/revoke"))
	case strings.HasSuffix(rest, "/connect") && !strings.Contains(strings.TrimSuffix(rest, "/connect"), "/"):
		s.connect(w, r, strings.TrimSuffix(rest, "/connect"))
	case !strings.Contains(rest, "/"):
		s.requestPage(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

type listData struct {
	Device  *Device
	Pending []persistence.AgentApprovalRequestRow
	Flash   string
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
	s.render(w, http.StatusOK, "list.html", listData{Device: d, Pending: pend, Flash: r.URL.Query().Get("done")})
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
		s.render(w, http.StatusOK, "request.html", s.requestData(req, ""))
	case http.MethodPost:
		s.decide(w, r, req)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) requestData(req *persistence.AgentApprovalRequestRow, notice string) requestData {
	var pretty bytes.Buffer
	if json.Indent(&pretty, req.Rendered, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(req.Rendered)
	}
	now := s.now()
	_, connect := s.connectEntry(*req)
	_, takesValue := s.valueEntry(req.Kind)
	takesValue = takesValue && !connect
	return requestData{Req: req, Pretty: pretty.String(), Notice: notice, TakesValue: takesValue, Connect: connect,
		Pending: req.Status == persistence.ApprovalPending && now.Before(req.ExpiresAt),
		Expired: req.Status == persistence.ApprovalExpired || (req.Status == persistence.ApprovalPending && !now.Before(req.ExpiresAt))}
}

func (s *Service) decide(w http.ResponseWriter, r *http.Request, req *persistence.AgentApprovalRequestRow) {
	d, _ := FromRequest(r)
	// A form is small; a value is at most MaxValueBytes (plan P4.2).
	r.Body = http.MaxBytesReader(w, r.Body, MaxValueBytes+4<<10)
	if err := r.ParseForm(); err != nil {
		s.render(w, http.StatusRequestEntityTooLarge, "request.html", s.requestData(req, "That was too long."))
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
	// Rotate first (design §9.2): a stale token (another tab rotated) is a
	// 409 and decides nothing.
	newTok, err := s.Rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Reload", "This device signed in again in another tab. Reload this page.")
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	SetCookie(w, r, newTok)
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
