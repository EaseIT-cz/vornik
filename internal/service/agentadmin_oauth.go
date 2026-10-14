package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
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
	// The account the person signed in to (design §22.2.7, §22.2.14 item 2;
	// GitHub #119 item 8). This closure is the provider identity's only
	// writer and runs inside the slot's effect: before the token is stored,
	// the old identity is cleared; after, the new one is written. So the
	// identity shown is always the stored token's account or none, and a
	// refused or failed decision writes nothing.
	signedInAs, paused := "", 0
	done := oauthCompletion{complete: func(ctx context.Context) error {
		s.signInMu.Lock()
		defer s.signInMu.Unlock()
		_, err := conn.CompleteWithIdentity(context.WithValue(ctx, oauthPublicationResultKey{}, &paused), state, code, func(_ context.Context, idToken string) error {
			signedInAs, _ = agentadmin.EmailFromIDToken(idToken)
			return nil
		})
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
	// A slot a device's connector install filed leaves the tools to the
	// connector page's second confirm (design §19.14.10, review 7b97 F2):
	// the marker is in the request the device decided, so an agent's
	// request_credential, which never sets it, keeps its tools request.
	var pl approvalPayload
	connector := ""
	if json.Unmarshal(req.Rendered, &pl) == nil {
		connector = pl.Connector
	}
	if connector == "" {
		s.background(func() { s.afterOAuthConnected(context.WithoutCancel(ctx), slot) })
	}
	// Say who signed in (design §22.2.7): the person chose the account at
	// the provider, and this is the one place Vornik can tell them.
	msg := "Vornik could not tell which account you signed in to; what your assistant said about this account was set aside."
	if signedInAs != "" {
		msg = "Signed in as " + signedInAs + "."
	}
	if paused > 0 {
		msg += fmt.Sprintf(" %d standing approval(s) that send from this account were paused; confirm them on the Standing approvals page if this is still the same account.", paused)
	}
	if connector != "" {
		ns, slug := slot.Namespace, strings.TrimPrefix(slot.Project, slot.Namespace+agentns.Separator)
		account := ""
		if a, err := s.grants.GetIntegration(ctx, slot.Project, slot.Server); err == nil {
			account = a.StoredAccountLabel()
		}
		link := "/ui/approve/connectors/" + url.PathEscape(connector) + "?" + url.Values{"namespace": {ns}, "project": {slug}, "account": {account}}.Encode()
		connectorPage(w, msg, link)
		return
	}
	page(http.StatusOK, "Connected", msg)
}

// connectorPage is the callback's page for a connector's sign-in: who
// signed in, and the link back to approve the tools (design §19.14.10).
func connectorPage(w http.ResponseWriter, msg, link string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><meta name=viewport content=\"width=device-width\"><title>Signed in</title><h1>Signed in</h1><p>" +
		html.EscapeString(msg) + "</p><p>One step left: <a href=\"" + html.EscapeString(link) + "\">approve the tools on the connector page</a>.</p>"))
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
			s.fileServerTools(ctx, slot.Namespace, p.ID, srv.Name, srv.URL, srv.Transport, srv.Auth, p.Permissions.Secrets)
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

// priorSignIn reports whether the server already holds a token row (so a
// sign-in replaces one) and the provider identity recorded for it, "" when
// none was provider-reported (C7, design §22.2.6). The row is read only for
// its existence; its tokens are not used.
func (s *agentAdminService) priorSignIn(ctx context.Context, project, server, integration string) (bool, string, error) {
	if s.c.repos == nil || s.c.repos.MCPOAuthTokens == nil {
		return false, "", errors.New("no token store")
	}
	_, err := s.c.repos.MCPOAuthTokens.Get(ctx, project, server)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return false, "", nil
	case err != nil:
		return false, "", err
	}
	a, err := s.grants.GetIntegration(ctx, project, integration)
	if err != nil {
		return true, "", nil // no row to read the identity from: not provably the same account
	}
	if a.IdentitySource != persistence.IdentitySourceProvider {
		return true, "", nil
	}
	return true, a.AccountIdentity, nil
}
