package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/brokeractions"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

type queuedRoundTripper func(*http.Request) (*http.Response, error)

func (f queuedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// #119 queued-action account binding: actual SQL generations, worker, service
// caller, Manager and physical MCP requests; the provider reads committed rows.
func TestQueuedActionExecution(t *testing.T) {
	for _, transport := range []string{"sse", "streamable-http"} {
		for _, auth := range []string{"static", "oauth", "none"} {
			for _, when := range []string{"queued", "queued-daemon", "resolving", "resolving-daemon", "after-final", "401", "403", "stable-refresh", "status-zero"} {
				if transport == "sse" && (when == "401" || when == "403" || when == "stable-refresh") {
					continue
				}
				if auth == "none" && (when == "401" || when == "403" || when == "stable-refresh" || when == "status-zero") {
					continue
				}
				t.Run(transport+"/"+auth+"/"+when, func(t *testing.T) {
					scope := "ns:hermes"
					if strings.HasSuffix(when, "-daemon") {
						scope = persistence.AccountDaemonScope
					}
					when = strings.TrimSuffix(when, "-daemon")
					ctx := context.Background()
					f := newAgentAdminFixture(t)
					const project = "hermes--comms"
					var mu sync.Mutex
					var received []string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, _ := io.ReadAll(r.Body)
						var req struct {
							ID     int64  `json:"id"`
							Method string `json:"method"`
						}
						_ = json.Unmarshal(raw, &req)
						result := any(map[string]any{})
						switch req.Method {
						case "initialize":
							result = map[string]any{"protocolVersion": "2024-11-05"}
						case "tools/list":
							result = map[string]any{"tools": []any{map[string]any{"name": "send"}}}
						case "tools/call":
							mu.Lock()
							received = append(received, r.Header.Get("Authorization"))
							n := len(received)
							mu.Unlock()
							if (when == "401" || when == "403" || when == "stable-refresh") && n == 1 {
								code := 401
								if when == "403" {
									code = 403
								}
								w.WriteHeader(code)
								return
							}
							result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "sent"}}}
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
					}))
					defer srv.Close()
					st, err := f.c.secretStoreForWrite()
					if err != nil {
						t.Fatal(err)
					}
					publish := func(value string, advance bool) {
						var publication persistence.AccountPublication
						if auth == "oauth" {
							tok := &persistence.MCPOAuthToken{ProjectID: project, ServerName: "mail-write", Resource: srv.URL, AccessToken: value, RefreshToken: "refresh", ExpiresAt: func() *time.Time { v := time.Now().Add(time.Hour); return &v }()}
							publication.Token, err = f.c.agentSealedTokens().PrepareSealedToken(tok)
						} else {
							publication.Secret, err = st.PreparePut("hermes", "MAIL_TOKEN", "secret", []byte(value), "device")
						}
						if err != nil {
							t.Fatal(err)
						}
						if !advance {
							if auth == "oauth" {
								err = f.c.repos.MCPOAuthTokens.Upsert(ctx, publication.Token)
							} else {
								err = f.c.repos.AgentSecrets.Upsert(ctx, *publication.Secret)
							}
							if err != nil {
								t.Fatal(err)
							}
							return
						}
						ticket, e := f.c.repos.AccountTransitions.Begin(ctx, scope, "fixture", "rotation", time.Now())
						if e != nil {
							t.Fatal(e)
						}
						if e = f.c.repos.AccountTransitions.Finish(ctx, ticket, publication); e != nil {
							t.Fatal(e)
						}
					}
					publish("Bob", true)
					var active bool
					var providerCalls int
					provider := func(ctx context.Context) (map[string]string, error) {
						if active {
							providerCalls++
							if when == "resolving" {
								active = false
								publish("Carol", true)
								active = true
							}
							if when == "status-zero" && providerCalls == 1 {
								return nil, &mcp.CallError{Class: mcp.FailureAuth}
							}
						}
						if auth == "none" {
							return nil, nil
						}
						var value string
						if auth == "oauth" {
							value, err = f.c.mcpConnector().AccessToken(ctx, mcpconnect.ServerRef{ProjectID: project, ServerName: "mail-write", URL: srv.URL})
						} else {
							var raw []byte
							raw, err = st.Get(ctx, "hermes", "MAIL_TOKEN")
							value = string(raw)
						}
						if err != nil {
							return nil, err
						}
						return map[string]string{"Authorization": "Bearer " + value}, nil
					}
					hc := *srv.Client()
					base := hc.Transport
					hc.Transport = queuedRoundTripper(func(r *http.Request) (*http.Response, error) {
						if active && when == "after-final" {
							active = false
							publish("Carol", true)
							active = true
						}
						return base.RoundTrip(r)
					})
					cfg := mcp.ServerConfig{Name: "mail-write", Transport: transport, URL: srv.URL, AllowedTools: []string{"send"}, AuthHeaderProvider: provider, HTTPClient: &hc}
					if when == "401" || when == "403" || when == "status-zero" {
						cfg.AuthInvalidator = func(context.Context) { publish("Carol", true) }
					}
					if when == "stable-refresh" {
						cfg.AuthInvalidator = func(context.Context) { publish("Bob-refreshed", false) }
					}
					manager := mcp.NewManager(zerolog.Nop())
					defer manager.Close()
					manager.StartForProject(ctx, project, []mcp.ServerConfig{cfg})
					if len(manager.Tools(project)) == 0 {
						t.Fatal("fixture manager has no tools")
					}
					now := time.Now().UTC()
					args := []byte(`{"to":"x@example.test"}`)
					sum, _ := approval.CanonicalSHA256(args)
					a := &persistence.BrokerAction{ActionID: "queued", ProjectID: project, TaskID: "task", WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail-write__send", Status: persistence.BrokerActionStaged, ArgsJSON: args, ArgsSHA256: sum, ExpiresAt: now.Add(time.Hour)}
					if _, err = f.c.repos.BrokerActions.Stage(ctx, a); err != nil {
						t.Fatal(err)
					}
					if _, err = f.c.repos.BrokerActions.PromoteStaged(ctx, "task"); err != nil {
						t.Fatal(err)
					}
					seed := *a
					seed.ActionID = "seed"
					seed.TaskID = "seed-task"
					if _, err = f.c.repos.BrokerActions.Stage(ctx, &seed); err != nil {
						t.Fatal(err)
					}
					if _, err = f.c.repos.BrokerActions.PromoteStaged(ctx, seed.TaskID); err != nil {
						t.Fatal(err)
					}
					g := &persistence.BrokerStandingGrant{ID: "grant", ProjectID: project, Namespace: "hermes", WorkflowID: a.WorkflowID, Action: a.ActionKind, KeyPaths: []string{}, KeyValuesSealed: "sealed", KeyHash: "key", MaxUses: 5, UsesLeft: 5, CreatedAt: now, ExpiresAt: now.Add(time.Hour), DigestThrough: now, DecisionGuard: seed.AccountGuard, SeedActionID: seed.ActionID}
					if err = f.c.repos.BrokerGrants.ApproveSeedAndCreate(ctx, seed.ActionID, sum, "device", g, 10, now); err != nil {
						t.Fatal(err)
					}
					if err = f.c.repos.BrokerGrants.ApproveUnderGrant(ctx, a.ActionID, sum, g.ID, g.KeyHash, now); err != nil {
						t.Fatal(err)
					}
					if when == "queued" {
						publish("Carol", true)
					}
					active = true
					worker := brokeractions.New(brokeractions.Config{Repo: f.c.repos.BrokerActions, Caller: actionCaller{c: f.c, mcp: manager}, WritesOn: func() bool { return true }, Logger: zerolog.Nop()})
					worker.Execute(ctx, a.ActionID)
					worker.Execute(ctx, a.ActionID)
					got, e := f.c.repos.BrokerActions.Get(ctx, a.ActionID)
					if e != nil {
						t.Fatal(e)
					}
					storedGrant, ge := f.c.repos.BrokerGrants.Get(ctx, g.ID)
					if ge != nil {
						t.Fatal(ge)
					}
					expectedUses := 4
					if when == "queued" || when == "resolving" || when == "status-zero" {
						expectedUses = 5
					}
					if storedGrant.UsesLeft != expectedUses {
						t.Fatalf("refund count %d want %d", storedGrant.UsesLeft, expectedUses)
					}
					mu.Lock()
					defer mu.Unlock()
					switch when {
					case "queued", "queued-daemon", "resolving", "resolving-daemon", "status-zero":
						if len(received) != 0 || got.Status != persistence.BrokerActionFailed || got.OutcomeClass != persistence.BrokerOutcomePreSendError {
							t.Fatalf("stale write: received=%v outcome=%s/%s", received, got.Status, got.OutcomeClass)
						}
					case "401", "403":
						if len(received) != 1 || received[0] != "Bearer Bob" || got.Status != persistence.BrokerActionFailed || got.OutcomeClass != persistence.BrokerOutcomeToolError {
							t.Fatalf("retry misreported: received=%v outcome=%s/%s", received, got.Status, got.OutcomeClass)
						}
					case "after-final":
						want := "Bearer Bob"
						if auth == "none" {
							want = ""
						}
						if len(received) != 1 || received[0] != want || got.Status != persistence.BrokerActionExecuted {
							t.Fatalf("old-auth boundary: received=%v outcome=%s/%s", received, got.Status, got.OutcomeClass)
						}
					case "stable-refresh":
						if len(received) != 2 || received[0] != "Bearer Bob" || received[1] != "Bearer Bob-refreshed" || got.Status != persistence.BrokerActionExecuted {
							t.Fatalf("same-account refresh: received=%v outcome=%s/%s", received, got.Status, got.OutcomeClass)
						}
					}
				})
			}
		}
	}
}

// A direct agent caller cannot recapture a current epoch or invent an action.
func TestQueuedActionCallerMissingContext(t *testing.T) {
	f := newAgentAdminFixture(t)
	_, _, err := (actionCaller{c: f.c}).CallToolOnce(context.Background(), "hermes--comms", "mcp__mail-write__send", `{}`)
	if !errors.Is(err, mcp.ErrNotSent) {
		t.Fatalf("missing private action context accepted: %v", err)
	}
}

func TestQueuedActionMissingRepositoryAndLegacy(t *testing.T) {
	for _, mode := range []string{"repository", "legacy", "partial"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgentAdminFixture(t)
			ctx := context.Background()
			now := time.Now()
			args := []byte(`{}`)
			sum, _ := approval.CanonicalSHA256(args)
			a := &persistence.BrokerAction{ActionID: "fail-closed", ProjectID: "hermes--comms", TaskID: "task", WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail-write__send", Status: persistence.BrokerActionStaged, ArgsJSON: args, ArgsSHA256: sum, ExpiresAt: now.Add(time.Hour)}
			if _, e := f.c.repos.BrokerActions.Stage(ctx, a); e != nil {
				t.Fatal(e)
			}
			if _, e := f.c.repos.BrokerActions.PromoteStaged(ctx, a.TaskID); e != nil {
				t.Fatal(e)
			}
			if e := f.c.repos.BrokerActions.Approve(ctx, a.ActionID, sum, "device", now); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "repository":
				f.c.repos.AccountTransitions = nil
			case "legacy":
				if _, e := f.c.DB.Exec(`UPDATE broker_actions SET account_ns_epoch=NULL,account_daemon_epoch=NULL`); e != nil {
					t.Fatal(e)
				}
			case "partial":
				if _, e := f.c.DB.Exec(`UPDATE broker_actions SET account_daemon_epoch=NULL`); e != nil {
					t.Fatal(e)
				}
			}
			worker := brokeractions.New(brokeractions.Config{Repo: f.c.repos.BrokerActions, Caller: actionCaller{c: f.c}, WritesOn: func() bool { return true }, Logger: zerolog.Nop()})
			worker.Execute(ctx, a.ActionID)
			got, e := f.c.repos.BrokerActions.Get(ctx, a.ActionID)
			if e != nil || got.Status != persistence.BrokerActionFailed || got.OutcomeClass != persistence.BrokerOutcomePreSendError {
				t.Fatalf("missing authority not refused: %+v %v", got, e)
			}
		})
	}
}
