package hermes

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// Phone is the scripted approver device of agent-administered Vornik's DoD
// lane (plan P8.1). It is not a mock: it uses the real device routes with a
// real device cookie, exactly as a phone's browser does. It pairs with the
// code vornikctl pair-device printed, reads the pending list and each
// request page, and decides by echoing the rendered sha256 the page showed.
type Phone struct {
	base   string
	client *http.Client
	// Shown is the rendered sha256 each decided request's page showed, for
	// the lane's cross-check against the stored row (review 20261002-69cc R4).
	Shown map[string]string
}

// PendingRequest is one card on the pending list.
type PendingRequest struct {
	ID       string
	Sentence string
}

// NewPhone builds a phone for the daemon at base.
func NewPhone(base string) *Phone {
	jar, _ := cookiejar.New(nil)
	return &Phone{base: strings.TrimRight(base, "/"), client: &http.Client{Jar: jar}, Shown: map[string]string{}}
}

// post sends a form as a same-origin browser POST.
func (p *Phone) post(path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, p.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", p.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return p.client.Do(req)
}

func (p *Phone) get(path string) (string, error) {
	resp, err := p.client.Get(p.base + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return string(b), nil
}

// Pair redeems a pairing code.
func (p *Phone) Pair(code string) error {
	resp, err := p.post("/ui/pair", url.Values{"code": {code}})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	// The redirect lands on the pending list, which needs the device cookie.
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/ui/approve/" {
		return fmt.Errorf("pairing: HTTP %d at %s", resp.StatusCode, resp.Request.URL.Path)
	}
	return nil
}

var (
	cardRE = regexp.MustCompile(`(?s)<div class="card"><p>(.*?)</p>.*?<a href="/ui/approve/([A-Za-z0-9_]+)">Review</a>`)
	shaRE  = regexp.MustCompile(`name="rendered_sha256" value="([^"]+)"`)
)

// Pending lists the requests waiting for a decision.
func (p *Phone) Pending() ([]PendingRequest, error) {
	page, err := p.get("/ui/approve/")
	if err != nil {
		return nil, err
	}
	var out []PendingRequest
	for _, m := range cardRE.FindAllStringSubmatch(page, -1) {
		out = append(out, PendingRequest{ID: m[2], Sentence: html.UnescapeString(m[1])})
	}
	return out, nil
}

// Approve approves a request; value is the credential for a credential
// request, "" otherwise.
func (p *Phone) Approve(id, value string) error { return p.decide(id, "approve", value) }

// Reject rejects a request.
func (p *Phone) Reject(id string) error { return p.decide(id, "reject", "") }

func (p *Phone) decide(id, decision, value string) error {
	page, err := p.get("/ui/approve/" + id)
	if err != nil {
		return err
	}
	m := shaRE.FindStringSubmatch(page)
	if m == nil {
		return fmt.Errorf("request %s: the page shows no decision form", id)
	}
	form := url.Values{"decision": {decision}, "rendered_sha256": {m[1]}}
	if value != "" {
		form.Set("value", value)
	}
	resp, err := p.post("/ui/approve/"+id, form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/ui/approve/" {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("deciding %s: HTTP %d at %s: %s", id, resp.StatusCode, resp.Request.URL.Path, b)
	}
	p.Shown[id] = m[1]
	return nil
}
