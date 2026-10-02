package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// OAuth for agent servers (agent-administered Vornik plan P4.4). The person
// taps Connect on the approver device; the sign-in comes back to the ONE
// redirect URI (/auth/mcp/callback), which dispatches on the attempt's
// origin. A device-started attempt is bound to the phone by a SameSite=Lax
// flow cookie (the device cookie is Strict and never arrives on the
// provider's cross-site redirect); an operator-started one goes to today's
// session-authenticated handler unchanged.

// flowCookie is the device flow's binding cookie.
const flowCookie = "vornik_oauth_flow"

const deviceOrigin = "device:"

// agentOAuthConnect is the ConnectEntry for an OAuth credential slot.
type agentOAuthConnect struct{ s *agentAdminService }

func oauthSlot(r persistence.AgentApprovalRequestRow) (*agentadmin.CredentialSlot, bool) {
	if r.Kind != persistence.ApprovalKindCredentialSlot {
		return nil, false
	}
	var pl approvalPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.Slot == nil || pl.Slot.Kind != agentadmin.CredentialOAuth ||
		pl.Slot.Namespace != r.Namespace || pl.Slot.Server == "" {
		return nil, false
	}
	return pl.Slot, true
}

func (a agentOAuthConnect) Applies(r persistence.AgentApprovalRequestRow) bool {
	_, ok := oauthSlot(r)
	return ok
}

// Start begins the sign-in for the slot's server. It decides nothing.
func (a agentOAuthConnect) Start(ctx context.Context, d *approverdevice.Device, r persistence.AgentApprovalRequestRow, shownSHA string) (string, *http.Cookie, error) {
	slot, ok := oauthSlot(r)
	if !ok || d == nil || r.Status != persistence.ApprovalPending {
		return "", nil, approverdevice.ErrNotDecidable // review 20261002-5276 F4
	}
	if subtle.ConstantTimeCompare([]byte(shownSHA), []byte(r.RenderedSHA256)) != 1 {
		return "", nil, approverdevice.ErrNotDecidable
	}
	conn := a.s.c.mcpConnector()
	if conn == nil {
		return "", nil, errors.New("no token store")
	}
	ref, ok := a.s.c.mcpServerRef(slot.Project, slot.Server)
	if !ok {
		return "", nil, errors.New("the server is not loaded")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	value := hex.EncodeToString(nonce)
	origin := deviceOrigin + d.ID + ":" + r.ID + ":" + r.RenderedSHA256
	begun, err := conn.BeginWith(ctx, ref, "device:"+d.ID, mcpconnect.BeginOptions{Origin: origin, FlowBinding: hashHex(value)})
	if err != nil {
		a.s.c.Logger.Warn().Err(err).Str("project", slot.Project).Str("server", slot.Server).Msg("agent oauth: the sign-in could not start")
		return "", nil, err
	}
	return begun.AuthorizationURL, &http.Cookie{Name: flowCookie, Value: value, Path: mcpconnect.CallbackPath,
		MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(a.s.c.publicOrigin(), "https://"), SameSite: http.SameSiteLaxMode}, nil
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mcpCallbackDispatch sends a device-started attempt to the device branch
// and everything else to operator, today's handler (unchanged).
func (c *Container) mcpCallbackDispatch(conn *mcpconnect.Connector, operator http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, binding, ok := conn.Peek(strings.TrimSpace(r.URL.Query().Get("state")))
		svc := c.agentAdmin()
		if !ok || !strings.HasPrefix(origin, deviceOrigin) || svc == nil {
			operator.ServeHTTP(w, r)
			return
		}
		svc.deviceOAuthCallback(w, r, conn, origin, binding)
	})
}

// deviceOAuthCallback completes a device-started sign-in. Order: the flow
// cookie, the device, the decision, then the exchange (decide, then store,
// as for a typed credential): a refused decision stores nothing, and an
// exchange that fails after it marks the request failed.
func (s *agentAdminService) deviceOAuthCallback(w http.ResponseWriter, r *http.Request, conn *mcpconnect.Connector, origin, binding string) {
	ctx := r.Context()
	q := r.URL.Query()
	page := func(status int, title, msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<!doctype html><meta name=viewport content=\"width=device-width\"><title>" + html.EscapeString(title) +
			"</title><h1>" + html.EscapeString(title) + "</h1><p>" + html.EscapeString(msg) + "</p><p><a href=\"/ui/approve/\">Back to approvals</a></p>"))
	}
	ck, err := r.Cookie(flowCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(hashHex(ck.Value)), []byte(binding)) != 1 {
		page(http.StatusForbidden, "Open this on your phone", "This sign-in was started on your approver device; finish it there.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: mcpconnect.CallbackPath, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	parts := strings.SplitN(strings.TrimPrefix(origin, deviceOrigin), ":", 3)
	if len(parts) != 3 {
		page(http.StatusBadRequest, "Not valid", "This sign-in cannot be completed.")
		return
	}
	devID, reqID, sha := parts[0], parts[1], parts[2]
	if vendorErr := strings.TrimSpace(q.Get("error")); vendorErr != "" {
		page(http.StatusBadRequest, "Not connected", "The sign-in was not completed. Nothing was changed; you can tap Connect again.")
		return
	}
	d, err := s.devices.ActiveDevice(ctx, devID)
	if err != nil {
		page(http.StatusForbidden, "Device not recognised", "The device that started this sign-in is no longer paired.")
		return
	}
	req, err := s.requests.GetRequest(ctx, reqID)
	if err != nil {
		page(http.StatusNotFound, "Not found", "There is no such request.")
		return
	}
	slot, ok := oauthSlot(*req)
	if !ok {
		page(http.StatusBadRequest, "Not valid", "This request is not a sign-in.")
		return
	}
	// The exchange runs inside the slot's effect (slotEffect), so a failure
	// after the decision is recorded on the request.
	state, code := strings.TrimSpace(q.Get("state")), strings.TrimSpace(q.Get("code"))
	done := oauthCompletion{complete: func(ctx context.Context) error {
		_, err := conn.Complete(ctx, state, code)
		return err
	}}
	err = s.devices.Decide(context.WithValue(ctx, oauthCompletionKey{}, done), d, reqID, sha, true)
	switch {
	case errors.Is(err, approverdevice.ErrPermanent):
		page(http.StatusBadGateway, "Not connected", "You approved this, but the sign-in did not complete. Ask your assistant to request it again.")
		return
	case err != nil:
		page(http.StatusConflict, "Already decided", "This request changed or was already decided. Nothing was connected.")
		return
	}
	s.background(func() { s.afterOAuthConnected(context.WithoutCancel(ctx), slot) })
	http.Redirect(w, r, "/ui/approve/?done=approved", http.StatusSeeOther)
}

// afterOAuthConnected lists the newly connected server's tools and files
// their approval (plan P4.3 trigger).
func (s *agentAdminService) afterOAuthConnected(ctx context.Context, slot *agentadmin.CredentialSlot) {
	p := s.c.Registry.GetProject(slot.Project)
	if p == nil {
		return
	}
	for _, srv := range p.MCP.Servers {
		if srv.Name == slot.Server {
			s.fileServerTools(ctx, slot.Namespace, p.ID, srv.Name, srv.URL, srv.Auth, p.Permissions.Secrets)
		}
	}
}

// agentOAuthHTTP is the connector's client for an agent project's server:
// discovery, registration, exchange and refresh go through the SSRF guard,
// because the server's own metadata names the endpoints.
func agentOAuthHTTP(projectID, serverURL string) *http.Client {
	if _, agent := agentns.FromID(projectID); !agent {
		return nil
	}
	return agentDialGuard(serverURL).HTTPClient(mcpOAuthHTTPTimeout)
}
