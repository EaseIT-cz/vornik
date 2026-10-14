package approverdevice

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// GitHub #314 (2026-10-13): the device's connector pages (agent-administered
// design §19.14.2, build decisions §19.14.8 item 7). The daemon supplies
// ConnectorPages; these tests pin what the page shows, what it passes on and
// what it refuses.

type fakeConnectorPages struct {
	mu        sync.Mutex
	confirms  []ConnectorChoice
	creds     []map[string][]byte
	shown     []string
	devices   []string
	projects  []string
	previewed []ConnectorChoice
	signIns   []string
	finishes  []ConnectorChoice
}

func (p *fakeConnectorPages) List(context.Context) ([]ConnectorView, error) {
	return []ConnectorView{{
		Name: "agenda", Title: "Calendar (read)", ReturnsPlain: "your events",
		Reads:     []ConnectorRead{{Server: "calendar", Reads: "your calendar", Host: "calendar-mcp.example", Tools: []string{"calendar_list_events"}}},
		Returns:   []string{"items[].title: text"},
		Variables: []ConnectorVariable{{Name: "range", Type: "enum", Values: []string{"today", "week"}, Default: "today"}},
		Installed: []string{"hermes--personal (work)"},
	}}, nil
}

func (p *fakeConnectorPages) Namespaces(context.Context) ([]ConnectorNamespace, error) {
	return []ConnectorNamespace{{Name: "hermes", Projects: []string{"personal"}}}, nil
}

func (p *fakeConnectorPages) Preview(_ context.Context, name string, ch ConnectorChoice) (ConnectorPreview, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.previewed = append(p.previewed, ch)
	if name != "agenda" {
		return ConnectorPreview{}, ErrNotAConnector
	}
	return ConnectorPreview{Sentence: "Your assistant (" + ch.Namespace + ") wants to install \"Calendar (read)\" for your " + ch.Account + " account",
		Shown: "abc123", Credentials: []ConnectorCredential{{Name: "CALENDAR_TOKEN"}}}, nil
}

func (p *fakeConnectorPages) CreateProject(_ context.Context, _ *Device, name, ns string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projects = append(p.projects, ns+"/"+name)
	return nil
}

func (p *fakeConnectorPages) Confirm(_ context.Context, d *Device, _ string, ch ConnectorChoice, shown string, creds map[string][]byte) (ConnectorOutcome, error) {
	if d == nil {
		return ConnectorOutcome{}, ErrNoDevice
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.confirms = append(p.confirms, ch)
	p.creds = append(p.creds, creds)
	p.shown = append(p.shown, shown)
	p.devices = append(p.devices, d.ID)
	return ConnectorOutcome{Installed: true, Checks: []ConnectorCheck{{Server: "calendar", Code: "ok"}}}, nil
}

func (p *fakeConnectorPages) Pending(_ context.Context, _ string, ch ConnectorChoice) (ConnectorPending, error) {
	if ch.Project != "personal" {
		return ConnectorPending{}, nil
	}
	return ConnectorPending{SignIn: []string{"mail"}, Ready: []string{"calendar"}, Sentence: "The server calendar ... signed in as alice@corp.example", Shown: "fin123"}, nil
}

func (p *fakeConnectorPages) SignIn(_ context.Context, d *Device, _ string, ch ConnectorChoice, server string) (ConnectorOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signIns = append(p.signIns, ch.Namespace+"/"+ch.Project+"/"+server+"/"+d.ID)
	return ConnectorOutcome{SignInURL: "https://provider.example/authorize?state=s1",
		SignInCookie: &http.Cookie{Name: "vornik_oauth_flow", Value: "v", Path: "/auth/mcp/callback"}}, nil
}

func (p *fakeConnectorPages) Finish(_ context.Context, _ *Device, _ string, ch ConnectorChoice, shown string) (ConnectorOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishes = append(p.finishes, ch)
	p.shown = append(p.shown, shown)
	return ConnectorOutcome{Installed: true, Checks: []ConnectorCheck{{Server: "calendar", Code: "ok"}}}, nil
}

// §19.14.10: the choose page shows what an earlier confirm left waiting; a
// sign-in POST sends the browser to the provider with the flow cookie; the
// finish POST forwards only the choice and the shown value.
func TestConnectorPage_OAuthSecondConfirm(t *testing.T) {
	_, p, phone := connectorFixture(t)
	_, body := phone.do(http.MethodGet, "/ui/approve/connectors/agenda?namespace=hermes&project=personal", nil, nil)
	for _, want := range []string{"Sign in for mail", "signed in as alice@corp.example", `name="shown" value="fin123"`, "/ui/approve/connectors/agenda/finish"} {
		if !strings.Contains(body, want) {
			t.Errorf("choose page lacks %q", want)
		}
	}
	res, _ := phone.do(http.MethodPost, "/ui/approve/connectors/agenda/signin", url.Values{"namespace": {"hermes"}, "project": {"personal"}, "server": {"mail"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "https://provider.example/authorize?state=s1" {
		t.Fatalf("sign-in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var flow bool
	for _, c := range res.Cookies() {
		flow = flow || c.Name == "vornik_oauth_flow"
	}
	if !flow || len(p.signIns) != 1 || !strings.HasPrefix(p.signIns[0], "hermes/personal/mail/") {
		t.Fatalf("flow cookie %v, sign-ins %v", flow, p.signIns)
	}
	res, body = phone.do(http.MethodPost, "/ui/approve/connectors/agenda/finish", url.Values{
		"namespace": {"hermes"}, "project": {"personal"}, "shown": {"fin123"}, "var_range": {"week"}, "tools": {"gmail_send"}}, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("finish: %d %s", res.StatusCode, body)
	}
	if len(p.finishes) != 1 || p.finishes[0].Variables != nil || p.finishes[0].Project != "personal" || p.shown[len(p.shown)-1] != "fin123" {
		t.Fatalf("finish forwarded %+v", p.finishes)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/connectors/agenda/signin", url.Values{"namespace": {"hermes"}, "project": {"personal"}, "server": {"../x"}}, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a bad server name: %d", res.StatusCode)
	}
}

func connectorFixture(t *testing.T) (*fixture, *fakeConnectorPages, *browser) {
	t.Helper()
	f := newFixture(t)
	p := &fakeConnectorPages{}
	f.svc.SetConnectorPages(p)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	return f, p, phone
}

// The list shows what each connector reads, in words AND as the exact
// tools the confirm approves, what it returns, and where it is installed.
func TestConnectorPage_ShowsReadsToolsAndInstalls(t *testing.T) {
	_, _, phone := connectorFixture(t)
	res, body := phone.do(http.MethodGet, "/ui/approve/connectors", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", res.StatusCode)
	}
	for _, want := range []string{"Calendar (read)", "your calendar", "calendar_list_events", "calendar-mcp.example", "items[].title", "hermes--personal (work)", "/ui/approve/connectors/agenda"} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
}

func TestConnectorPage_RequiresDevice(t *testing.T) {
	_, p, phone := connectorFixture(t)
	stranger := newBrowser(t, phone.srv)
	for _, path := range []string{"/ui/approve/connectors", "/ui/approve/connectors/agenda"} {
		res, _ := stranger.do(http.MethodGet, path, nil, nil)
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/ui/pair") {
			t.Errorf("GET %s without a device: %d", path, res.StatusCode)
		}
	}
	form := url.Values{"namespace": {"hermes"}, "project": {"personal"}, "shown": {"abc123"}, "credential_CALENDAR_TOKEN": {"v"}}
	res, _ := stranger.do(http.MethodPost, "/ui/approve/connectors/agenda", form, map[string]string{"Cookie": "vornik_session=operator", "X-API-Key": "sk-vornik-admin"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("POST without a device: %d", res.StatusCode)
	}
	res, _ = phone.do(http.MethodPost, "/ui/approve/connectors/agenda", form, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", res.StatusCode)
	}
	if len(p.confirms) != 0 || len(p.projects) != 0 {
		t.Fatalf("a refused request reached the daemon: %v %v", p.confirms, p.projects)
	}
}

// The choose page shows the sentence for the chosen values and carries the
// shown value and one password field per credential.
func TestConnectorPage_ChooseShowsTheSentence(t *testing.T) {
	_, _, phone := connectorFixture(t)
	res, body := phone.do(http.MethodGet, "/ui/approve/connectors/agenda?namespace=hermes&project=personal&account=work", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("choose: %d", res.StatusCode)
	}
	for _, want := range []string{"for your work account", `name="shown" value="abc123"`, `type="password" name="credential_CALENDAR_TOKEN"`, `value="hermes"`} {
		if !strings.Contains(body, want) {
			t.Errorf("choose page lacks %q", want)
		}
	}
	res, _ = phone.do(http.MethodGet, "/ui/approve/connectors/nope", nil, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown connector: %d", res.StatusCode)
	}
}

// A forged form field (a URL, a tool, a variable the connector does not
// offer) never reaches the daemon; the device is the one that confirmed.
func TestConnectorPage_ConfirmIgnoresForgedFields(t *testing.T) {
	f, p, phone := connectorFixture(t)
	form := url.Values{
		"namespace": {"hermes"}, "project": {"personal"}, "account": {"work"}, "shown": {"abc123"},
		"var_range": {"week"}, "var_calendar_server_url": {"https://evil.example/mcp"},
		"url": {"https://evil.example/mcp"}, "tool": {"gmail_send"}, "tools": {"gmail_send"},
		"credential_CALENDAR_TOKEN": {"secret-value"},
	}
	res, body := phone.do(http.MethodPost, "/ui/approve/connectors/agenda", form, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "ok") {
		t.Fatalf("confirm: %d %s", res.StatusCode, body)
	}
	if len(p.confirms) != 1 {
		t.Fatalf("confirms %d", len(p.confirms))
	}
	ch := p.confirms[0]
	if ch.Namespace != "hermes" || ch.Project != "personal" || ch.Account != "work" || len(ch.Variables) != 1 || ch.Variables["range"] != "week" {
		t.Fatalf("choice %+v", ch)
	}
	if string(p.creds[0]["CALENDAR_TOKEN"]) != "secret-value" || len(p.creds[0]) != 1 || p.shown[0] != "abc123" {
		t.Fatalf("credentials %v shown %v", p.creds, p.shown)
	}
	if strings.Contains(body, "secret-value") || strings.Contains(body, "evil.example") {
		t.Fatal("the page echoed a value it was sent")
	}
	devs, _ := f.svc.ListDevices(context.Background())
	if len(devs) != 1 || p.devices[0] != devs[0].ID {
		t.Fatalf("confirmed by %v", p.devices)
	}
}

// The project button creates only the connector's own project.
func TestConnectorPage_CreateProject(t *testing.T) {
	_, p, phone := connectorFixture(t)
	res, _ := phone.do(http.MethodPost, "/ui/approve/connectors/agenda/project", url.Values{"namespace": {"hermes"}, "project": {"other"}}, nil)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "project=agenda") {
		t.Fatalf("create project: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if len(p.projects) != 1 || p.projects[0] != "hermes/agenda" {
		t.Fatalf("projects %v", p.projects)
	}
}

func TestConnectorPage_UnavailableWithoutBackend(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	res, _ := phone.do(http.MethodGet, "/ui/approve/connectors", nil, nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no backend: %d", res.StatusCode)
	}
}
