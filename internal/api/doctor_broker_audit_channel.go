package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// BrokerTarget is one project's trading broker, as the registry wires it.
type BrokerTarget struct {
	ProjectID string
	BaseURL   string
}

// SetBrokerTargets wires the source of broker targets for the
// broker_audit_channel check. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetBrokerTargets(src func() []BrokerTarget) {
	h.brokerTargets = src
}

// brokerAuditHealth is the part of the broker's /health this check reads.
// Pointers, so a broker that does not report a field is "not examined"
// rather than healthy.
type brokerAuditHealth struct {
	AuditChannelAuthOK *bool  `json:"audit_channel_auth_ok"`
	AuditPending       *int64 `json:"audit_pending"`
}

// checkBrokerAuditChannel reads each trading broker's view of its audit
// channel to the daemon (trading-broker-design.md, amendment 2026-10-23):
// a rejected key means orders are placed but never recorded (ERROR); a
// backlog means events are not landing (WARNING); a broker that cannot be
// read is not examined (WARNING), never OK. Read-only.
func (h *DoctorHandlers) checkBrokerAuditChannel(ctx context.Context) DoctorCheck {
	const name = "broker_audit_channel"
	if h.brokerTargets == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "broker targets not wired"}
	}
	// One probe per broker, naming every project it serves.
	projectsByURL := map[string][]string{}
	for _, t := range h.brokerTargets() {
		if t.BaseURL == "" {
			continue
		}
		projectsByURL[t.BaseURL] = append(projectsByURL[t.BaseURL], t.ProjectID)
	}
	if len(projectsByURL) == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no project wires a broker MCP server"}
	}
	urls := make([]string, 0, len(projectsByURL))
	for u := range projectsByURL {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	status := "OK"
	unreachable := 0
	var items []string
	for _, u := range urls {
		who := fmt.Sprintf("%s (projects %s)", u, strings.Join(projectsByURL[u], ", "))
		health, err := fetchBrokerAuditHealth(ctx, u)
		switch {
		case err != nil:
			unreachable++
			status = worseDoctorStatus(status, "WARNING")
			items = append(items, who+": not examined — "+err.Error())
		case !*health.AuditChannelAuthOK:
			status = "ERROR"
			items = append(items, fmt.Sprintf("%s: the daemon rejects the broker's key, so trades are placed but not recorded "+
				"(%d events journaled); recreate broker-mcp with the current VORNIK_BROKER_DAEMON_API_KEY "+
				"(a restart keeps the old key)", who, *health.AuditPending))
		case *health.AuditPending > 0:
			status = worseDoctorStatus(status, "WARNING")
			items = append(items, fmt.Sprintf("%s: %d audit events are not reaching the daemon; check the broker's "+
				"VORNIK_BROKER_DAEMON_URL from inside the trading network (make trading-preflight)", who, *health.AuditPending))
		}
	}
	scope := fmt.Sprintf("%d broker(s) examined, %d not reachable", len(urls), unreachable)
	if status == "OK" {
		return DoctorCheck{Name: name, Status: "OK", Message: "every broker's audit channel is accepted and drained (" + scope + ")"}
	}
	return DoctorCheck{Name: name, Status: status, Message: "broker audit channel needs attention (" + scope + ")", Items: items}
}

// worseDoctorStatus keeps the more severe of two statuses.
func worseDoctorStatus(a, b string) string {
	rank := map[string]int{"OK": 0, "WARNING": 1, "ERROR": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func fetchBrokerAuditHealth(ctx context.Context, baseURL string) (*brokerAuditHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /health: HTTP %d", resp.StatusCode)
	}
	var health brokerAuditHealth
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&health); err != nil {
		return nil, fmt.Errorf("unparseable /health: %w", err)
	}
	if health.AuditChannelAuthOK == nil || health.AuditPending == nil {
		return nil, fmt.Errorf("/health reports no audit_channel_auth_ok/audit_pending (broker older than 2026-08-19?)")
	}
	return &health, nil
}
