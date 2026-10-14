package approverdevice

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The connector pages (agent-administered design §19.14.2, build decisions
// §19.14.8 item 7; GitHub #314). A person on the paired device installs a
// vetted read-only connector themselves: list, choose (namespace, project,
// account, the connector's few variables, its credential), confirm. This
// package only shows and forwards: the daemon supplies ConnectorPages, and
// every URL, tool, role and instruction comes from its vetted catalogue,
// never from the form. Every route is behind RequireDevice, every POST
// passes approval.CheckRequest there, and the confirm rotates the device
// token as a decision does.

// ErrNotAConnector refuses a name the device may not install.
var ErrNotAConnector = errors.New("approverdevice: not a connector this device can install")

// ConnectorRead is one server a connector reads: in words, and the exact
// tools the confirm approves.
type ConnectorRead struct {
	Server, Reads, Host string
	Tools               []string
}

// ConnectorVariable is one variable the form fills.
type ConnectorVariable struct {
	Name, Type, Help, Default string
	Values                    []string
}

// ConnectorView is one connector on the list.
type ConnectorView struct {
	Name, Title, ReturnsPlain string
	Reads                     []ConnectorRead
	Returns                   []string
	Variables                 []ConnectorVariable
	// Installed names each install, "<project> (<account>)".
	Installed []string
}

// ConnectorNamespace is one agent namespace and its projects.
type ConnectorNamespace struct {
	Name     string
	Projects []string
}

// ConnectorChoice is what the person chose. Variables holds only the
// connector's own variables; the page drops anything else.
type ConnectorChoice struct {
	Namespace, Project, Account string
	Variables                   map[string]string
}

// ConnectorCredential is one credential the confirm stores.
type ConnectorCredential struct {
	Name   string
	Stored bool // already stored: the field may be left blank
}

// ConnectorPreview is the sentence for a choice, and the value that binds
// the confirm to it.
type ConnectorPreview struct {
	Sentence, Shown, Refused string
	Credentials              []ConnectorCredential
	// ProjectMissing: the chosen project does not exist yet; the page offers
	// to create the connector's own project.
	ProjectMissing bool
	// SignInAfter: a provider sign-in follows the confirm, then a second
	// confirm approves the tools (design §19.14.10).
	SignInAfter bool
}

// ConnectorCheck is one server's connection check.
type ConnectorCheck struct{ Server, Code string }

// ConnectorOutcome is what a confirm did.
type ConnectorOutcome struct {
	Installed bool
	// Refused: nothing was filed, and why. Sentence is the current sentence
	// when the shown one is stale.
	Refused, Sentence string
	// Failed names the step that stopped the confirm after the install
	// (credential, tools), with FailedWhy in words.
	Failed, FailedWhy string
	Checks            []ConnectorCheck
	// SignInURL and SignInCookie start a provider sign-in (design
	// §19.14.10): the page sets the cookie and sends the browser there.
	SignInURL    string
	SignInCookie *http.Cookie
}

// ConnectorPending is what a device install still waits for (design
// §19.14.10): servers to sign in to, and servers whose sign-in is done,
// whose tools the second confirm approves with Sentence, bound by Shown.
type ConnectorPending struct {
	SignIn, Ready   []string
	Sentence, Shown string
}

// ConnectorPages is the daemon's connector surface for the device.
type ConnectorPages interface {
	List(ctx context.Context) ([]ConnectorView, error)
	Namespaces(ctx context.Context) ([]ConnectorNamespace, error)
	Preview(ctx context.Context, name string, ch ConnectorChoice) (ConnectorPreview, error)
	CreateProject(ctx context.Context, d *Device, name, namespace string) error
	Confirm(ctx context.Context, d *Device, name string, ch ConnectorChoice, shown string, credentials map[string][]byte) (ConnectorOutcome, error)
	// Pending, SignIn and Finish are the OAuth second confirm (§19.14.10).
	Pending(ctx context.Context, name string, ch ConnectorChoice) (ConnectorPending, error)
	SignIn(ctx context.Context, d *Device, name string, ch ConnectorChoice, server string) (ConnectorOutcome, error)
	Finish(ctx context.Context, d *Device, name string, ch ConnectorChoice, shown string) (ConnectorOutcome, error)
}

// SetConnectorPages installs the connector pages' backend.
func (s *Service) SetConnectorPages(p ConnectorPages) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectors = p
}

func (s *Service) connectorPages() ConnectorPages {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connectors
}

var (
	connectorNameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	connectorFieldRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,128}$`)
	credentialNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type connectorsData struct{ Connectors []ConnectorView }

type connectorData struct {
	Connector  ConnectorView
	Namespaces []ConnectorNamespace
	Choice     ConnectorChoice
	Preview    *ConnectorPreview
	Pending    *ConnectorPending
	Error      string
}

type connectorDoneData struct {
	Connector ConnectorView
	Choice    ConnectorChoice
	Outcome   ConnectorOutcome
}

// connectorsRouter serves everything under /ui/approve/connectors.
func (s *Service) connectorsRouter(w http.ResponseWriter, r *http.Request, rest string) {
	p := s.connectorPages()
	if p == nil {
		s.renderStatus(w, http.StatusServiceUnavailable, "Connectors are not available", "Connectors are not available on this daemon.")
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		views, err := p.List(r.Context())
		if err != nil {
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		s.render(w, http.StatusOK, "connectors.html", connectorsData{Connectors: views})
		return
	}
	name, action, _ := strings.Cut(rest, "/")
	if !connectorNameRe.MatchString(name) || (action != "" && action != "project" && action != "finish" && action != "signin") {
		http.NotFound(w, r)
		return
	}
	view, ok, err := connectorOf(r.Context(), p, name)
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	if !ok {
		s.renderStatus(w, http.StatusNotFound, "Not found", "This is not a connector this device can install.")
		return
	}
	switch {
	case (action == "finish" || action == "signin") && r.Method == http.MethodPost:
		s.connectorFinish(w, r, p, view, action)
	case action == "project" && r.Method == http.MethodPost:
		s.connectorProject(w, r, p, view)
	case action == "" && r.Method == http.MethodPost:
		s.connectorConfirm(w, r, p, view)
	case action == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.connectorChoose(w, r, p, view)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func connectorOf(ctx context.Context, p ConnectorPages, name string) (ConnectorView, bool, error) {
	views, err := p.List(ctx)
	if err != nil {
		return ConnectorView{}, false, err
	}
	for _, v := range views {
		if v.Name == name {
			return v, true, nil
		}
	}
	return ConnectorView{}, false, nil
}

// choiceFrom reads the choice from a query or a form: the namespace,
// project and account, and only the connector's own variables (var_<name>).
// Any other field is ignored, never echoed.
func choiceFrom(v url.Values, view ConnectorView) ConnectorChoice {
	field := func(k string) string {
		s := strings.TrimSpace(v.Get(k))
		if !connectorFieldRe.MatchString(s) {
			return ""
		}
		return s
	}
	ch := ConnectorChoice{Namespace: field("namespace"), Project: field("project"), Account: strings.ToLower(field("account"))}
	for _, cv := range view.Variables {
		if val := strings.TrimSpace(v.Get("var_" + cv.Name)); val != "" && len(val) <= 200 && !strings.ContainsAny(val, "\r\n") {
			if ch.Variables == nil {
				ch.Variables = map[string]string{}
			}
			ch.Variables[cv.Name] = val
		}
	}
	return ch
}

// connectorChoose serves GET /ui/approve/connectors/<name>: the form, and
// with a namespace and project chosen, the sentence the confirm approves.
func (s *Service) connectorChoose(w http.ResponseWriter, r *http.Request, p ConnectorPages, view ConnectorView) {
	nss, err := p.Namespaces(r.Context())
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	data := connectorData{Connector: view, Namespaces: nss, Choice: choiceFrom(r.URL.Query(), view)}
	if data.Choice.Namespace != "" && data.Choice.Project != "" {
		pv, err := p.Preview(r.Context(), view.Name, data.Choice)
		switch {
		case errors.Is(err, ErrNotAConnector):
			s.renderStatus(w, http.StatusNotFound, "Not found", "This is not a connector this device can install.")
			return
		case err != nil:
			s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
			return
		}
		data.Preview = &pv
		// What an earlier confirm left waiting (design §19.14.10).
		if pend, err := p.Pending(r.Context(), view.Name, data.Choice); err == nil && len(pend.SignIn)+len(pend.Ready) > 0 {
			data.Pending = &pend
		}
	}
	s.render(w, http.StatusOK, "connector.html", data)
}

// connectorProject serves POST /ui/approve/connectors/<name>/project: it
// creates the connector's own project in the chosen namespace (design
// §19.14.8 item 5a), and returns to the choose page with it selected.
func (s *Service) connectorProject(w http.ResponseWriter, r *http.Request, p ConnectorPages, view ConnectorView) {
	d, _ := FromRequest(r)
	if r.ParseForm() != nil {
		s.renderStatus(w, http.StatusBadRequest, "Not created", "The form could not be read.")
		return
	}
	ch := choiceFrom(r.PostForm, view)
	if ch.Namespace == "" {
		s.renderStatus(w, http.StatusBadRequest, "Not created", "Choose a namespace first.")
		return
	}
	if err := p.CreateProject(r.Context(), d, view.Name, ch.Namespace); err != nil {
		s.renderStatus(w, http.StatusConflict, "Not created", "The project could not be created: "+err.Error())
		return
	}
	q := url.Values{"namespace": {ch.Namespace}, "project": {view.Name}, "account": {ch.Account}}
	for k, v := range ch.Variables {
		q.Set("var_"+k, v)
	}
	http.Redirect(w, r, "/ui/approve/connectors/"+view.Name+"?"+q.Encode(), http.StatusSeeOther)
}

// connectorConfirm serves POST /ui/approve/connectors/<name>: one
// submission installs, stores the credential and approves the tools.
func (s *Service) connectorConfirm(w http.ResponseWriter, r *http.Request, p ConnectorPages, view ConnectorView) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*MaxValueBytes+8<<10)
	if err := r.ParseForm(); err != nil {
		s.renderStatus(w, http.StatusRequestEntityTooLarge, "Not installed", "That was too long.")
		return
	}
	ch := choiceFrom(r.PostForm, view)
	creds := map[string][]byte{}
	for k, vs := range r.PostForm {
		name, ok := strings.CutPrefix(k, "credential_")
		if !ok || !credentialNameRe.MatchString(name) || len(vs) != 1 || vs[0] == "" {
			continue
		}
		if len(vs[0]) > MaxValueBytes {
			s.renderStatus(w, http.StatusRequestEntityTooLarge, "Not installed", "A value is too long.")
			return
		}
		creds[name] = []byte(vs[0])
	}
	shown := r.PostFormValue("shown")
	s.connectorDecision(w, r, view, ch, func(d *Device) (ConnectorOutcome, error) {
		return p.Confirm(r.Context(), d, view.Name, ch, shown, creds)
	})
}

// connectorFinish serves POST /ui/approve/connectors/<name>/finish (the
// OAuth second confirm: only namespace, project, account and the shown
// value reach the daemon) and .../signin (start a provider sign-in for one
// waiting server, named by the form). Design §19.14.10.
func (s *Service) connectorFinish(w http.ResponseWriter, r *http.Request, p ConnectorPages, view ConnectorView, action string) {
	if r.ParseForm() != nil {
		s.renderStatus(w, http.StatusBadRequest, "Not changed", "The form could not be read.")
		return
	}
	ch := choiceFrom(r.PostForm, view)
	ch.Variables = nil
	shown, server := r.PostFormValue("shown"), r.PostFormValue("server")
	if action == "signin" && !connectorFieldRe.MatchString(server) {
		http.NotFound(w, r)
		return
	}
	s.connectorDecision(w, r, view, ch, func(d *Device) (ConnectorOutcome, error) {
		if action == "signin" {
			return p.SignIn(r.Context(), d, view.Name, ch, server)
		}
		return p.Finish(r.Context(), d, view.Name, ch, shown)
	})
}

// connectorDecision rotates the device token as a decision does (design
// §9.2), runs call, and either sends the browser to a provider sign-in or
// renders the outcome.
func (s *Service) connectorDecision(w http.ResponseWriter, r *http.Request, view ConnectorView, ch ConnectorChoice, call func(*Device) (ConnectorOutcome, error)) {
	d, _ := FromRequest(r)
	rot, err := s.rotate(r.Context(), d, tokenFromRequest(r))
	if errors.Is(err, ErrStaleDevice) {
		s.renderStatus(w, http.StatusConflict, "Signed in elsewhere", staleMessage)
		return
	}
	if err != nil {
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	defer s.closeShare(r.Context(), d.ID, rot)
	w = s.withCookie(w, r, rot.token)
	out, err := call(d)
	if err == nil && out.SignInURL != "" && out.SignInCookie != nil {
		http.SetCookie(w, out.SignInCookie)
		http.Redirect(w, r, out.SignInURL, http.StatusSeeOther)
		return
	}
	switch {
	case errors.Is(err, ErrNotAConnector):
		s.renderStatus(w, http.StatusNotFound, "Not found", "This is not a connector this device can install.")
		return
	case err != nil:
		s.renderStatus(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	s.render(w, http.StatusOK, "connector_done.html", connectorDoneData{Connector: view, Choice: ch, Outcome: out})
}
