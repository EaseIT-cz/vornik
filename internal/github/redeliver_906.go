package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The release-time redelivery pass (GitHub #906). GitHub does not retry a
// failed webhook delivery, so a delivery the held daemon refused with 503 is
// lost unless redelivered. After a release the daemon lists each App's
// deliveries in the hold's window and redelivers those that failed because
// the daemon was unavailable.
// see LLD § https://docs.vornik.io §13.13

// The pass's bounds (§13.13). Variables so a test can shrink them.
var (
	maxRedeliveryPages      = 10
	maxRedeliveries         = 200
	redeliveryDeadline      = 2 * time.Minute
	redeliveryRequestBudget = 15 * time.Second
)

// maxDeliveriesPageBytes caps one page read: 100 deliveries of metadata are
// about 40 KiB.
const maxDeliveriesPageBytes = 1 << 20

// maxRedeliveryErrors caps the error strings a report keeps.
const maxRedeliveryErrors = 5

// RedeliveryReport is what the pass did, for the log, the audit row and
// GET /api/v1/daemon/hold.
type RedeliveryReport struct {
	State                  string    `json:"state"` // running, done, not_configured
	WindowFrom             time.Time `json:"window_from"`
	WindowTo               time.Time `json:"window_to"`
	WindowFromProcessStart bool      `json:"window_from_process_start,omitempty"`
	Apps                   int       `json:"apps"`
	Examined               int       `json:"examined"`
	Failed                 int       `json:"failed"`
	Redelivered            int       `json:"redelivered"`
	NotRedelivered         int       `json:"not_redelivered"`
	NotUnavailability      int       `json:"not_unavailability"`
	Truncated              bool      `json:"truncated,omitempty"`
	Errors                 []string  `json:"errors,omitempty"`
}

func (r *RedeliveryReport) addError(err error) {
	if len(r.Errors) < maxRedeliveryErrors {
		r.Errors = append(r.Errors, err.Error())
	}
}

// hookDelivery is the part of GET /app/hook/deliveries the pass reads.
type hookDelivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	StatusCode  int       `json:"status_code"`
}

// appKey is one GitHub App: the deliveries list is per App, not per
// installation.
type appKey struct {
	appID      int64
	privateKey *rsa.PrivateKey
	keyHash    string
	registered bool
}

// apps returns the distinct Apps the channel holds a private key for.
func (c *Channel) apps() []appKey {
	var out []appKey
	seen := map[int64]bool{}
	for _, inst := range c.installations {
		if inst.appID == 0 || inst.privateKey == nil || seen[inst.appID] {
			continue
		}
		seen[inst.appID] = true
		out = append(out, appKey{appID: inst.appID, privateKey: inst.privateKey, keyHash: inst.keyHash, registered: inst.registered})
	}
	return out
}

// HasAppKey reports whether the channel can list its App's deliveries.
func (c *Channel) HasAppKey() bool { return len(c.apps()) > 0 }

// RedeliverFailed redelivers each delivery in [from, to] whose latest
// attempt failed with no response, 502, 503 or 504 and that no attempt
// delivered (§13.13). It never returns an error: what failed is in the
// report.
func (c *Channel) RedeliverFailed(ctx context.Context, from, to time.Time) RedeliveryReport {
	rep := RedeliveryReport{State: "done", WindowFrom: from, WindowTo: to}
	apps := c.apps()
	if len(apps) == 0 {
		rep.State = "not_configured"
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, redeliveryDeadline)
	defer cancel()
	budget := maxRedeliveries
	for _, app := range apps {
		rep.Apps++
		c.redeliverApp(ctx, app, from, to, &budget, &rep)
	}
	rep.NotRedelivered = rep.Failed - rep.Redelivered
	return rep
}

// redeliverApp runs the pass for one App: list the window, then redeliver.
func (c *Channel) redeliverApp(ctx context.Context, app appKey, from, to time.Time, budget *int, rep *RedeliveryReport) {
	if err := GuardKey(app.keyHash, app.registered); err != nil {
		rep.addError(fmt.Errorf("app %d: %w", app.appID, err))
		return
	}
	// Listing gets half of the time left, so a busy App still leaves time
	// to redeliver, and a second App still gets some (reviews 21bd #2,
	// d94e #2).
	listBudget := redeliveryDeadline / 2
	if dl, ok := ctx.Deadline(); ok {
		listBudget = time.Until(dl) / 2
	}
	listCtx, cancel := context.WithTimeout(ctx, listBudget)
	window, err := c.listWindow(listCtx, app, from, to, rep)
	cancel()
	if err != nil {
		rep.addError(fmt.Errorf("app %d: %w", app.appID, err))
	}
	// Group by guid; the list is newest first, so the first attempt seen of
	// a guid is its latest.
	latest := map[string]hookDelivery{}
	delivered := map[string]bool{}
	var order []string
	for _, d := range window {
		if d.StatusCode/100 == 2 {
			delivered[d.GUID] = true
		}
		if _, ok := latest[d.GUID]; !ok {
			latest[d.GUID] = d
			order = append(order, d.GUID)
		}
	}
	// A guid with any 2xx attempt is done. Should GitHub ever list a later
	// failed attempt of a delivered guid, the task creator's idempotency key
	// github-app:<guid> still makes the redelivery a no-op (review d94e #3).
	for _, guid := range order {
		if delivered[guid] {
			continue
		}
		rep.Failed++
		d := latest[guid]
		if !unavailability(d.StatusCode) {
			rep.NotUnavailability++
			continue
		}
		if *budget == 0 || ctx.Err() != nil {
			rep.Truncated = true
			continue
		}
		*budget--
		if err := c.redeliver(ctx, app, d.ID); err != nil {
			rep.addError(fmt.Errorf("app %d delivery %d: %w", app.appID, d.ID, err))
			continue
		}
		rep.Redelivered++
	}
}

// unavailability is a failure the daemon's absence explains: no response,
// or a gateway's or the held gate's answer. Any other failure was the
// daemon's own answer to the payload, which a redelivery repeats.
func unavailability(status int) bool {
	switch status {
	case 0, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// listWindow pages GET /app/hook/deliveries until a delivery older than
// from, the page cap or the deadline. It returns the deliveries in the
// window read so far, with any error.
func (c *Channel) listWindow(ctx context.Context, app appKey, from, to time.Time, rep *RedeliveryReport) ([]hookDelivery, error) {
	var out []hookDelivery
	next := c.apiBaseURL + "/app/hook/deliveries?per_page=100"
	for page := 0; next != ""; page++ {
		if page == maxRedeliveryPages || ctx.Err() != nil {
			rep.Truncated = true
			return out, nil
		}
		ds, link, err := c.deliveriesPage(ctx, app, next)
		if err != nil && ctx.Err() != nil {
			// The listing's time ran out mid-page: a bound, not a failure.
			rep.Truncated = true
			return out, nil
		}
		if err != nil {
			return out, err
		}
		for _, d := range ds {
			if d.DeliveredAt.Before(from) {
				return out, nil
			}
			if d.DeliveredAt.After(to) {
				continue
			}
			rep.Examined++
			out = append(out, d)
		}
		next = nextLink(link, c.apiBaseURL)
	}
	return out, nil
}

func (c *Channel) deliveriesPage(ctx context.Context, app appKey, pageURL string) ([]hookDelivery, string, error) {
	resp, err := c.appRequest(ctx, app, http.MethodGet, pageURL)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDeliveriesPageBytes))
	if err != nil {
		return nil, "", fmt.Errorf("list deliveries: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("list deliveries HTTP %d: %s", resp.StatusCode, truncateErrorBody(string(body)))
	}
	var ds []hookDelivery
	if err := json.Unmarshal(body, &ds); err != nil {
		return nil, "", fmt.Errorf("list deliveries: parse: %w", err)
	}
	return ds, resp.Header.Get("Link"), nil
}

func (c *Channel) redeliver(ctx context.Context, app appKey, id int64) error {
	resp, err := c.appRequest(ctx, app, http.MethodPost, fmt.Sprintf("%s/app/hook/deliveries/%d/attempts", c.apiBaseURL, id))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("redeliver HTTP %d: %s", resp.StatusCode, truncateErrorBody(string(body)))
	}
	return nil
}

// appRequest sends one request with the App's JWT and its own deadline.
func (c *Channel) appRequest(ctx context.Context, app appKey, method, target string) (*http.Response, error) {
	jwt, err := signJWT(app.appID, app.privateKey, time.Now())
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, redeliveryRequestBudget)
	req, err := http.NewRequestWithContext(rctx, method, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// nextLink returns the Link header's rel="next" URL when it is on the
// configured API host, else "" (a cursor that points elsewhere is not
// followed with the App's JWT).
func nextLink(link, apiBase string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 || !strings.Contains(part, `rel="next"`) {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		u, err := url.Parse(raw)
		base, berr := url.Parse(apiBase)
		if err != nil || berr != nil || u.Host != base.Host || u.Scheme != base.Scheme {
			continue // never send the App's JWT to another host (review d94e #4)
		}
		return raw
	}
	return ""
}
