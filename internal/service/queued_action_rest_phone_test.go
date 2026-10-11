package service

import (
	"context"
	"errors"
	"github.com/rs/zerolog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/brokeractions"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

type queuedSecrets struct {
	persistence.AgentSecretRepository
	afterGet func()
}

func (r *queuedSecrets) Get(ctx context.Context, ns, name string) (*persistence.AgentSecretRow, error) {
	row, err := r.AgentSecretRepository.Get(ctx, ns, name)
	if r.afterGet != nil {
		f := r.afterGet
		r.afterGet = nil
		f()
	}
	return row, err
}

type queuedIntegration struct {
	persistence.AgentGrantRepository
	afterGet func()
}

func (r *queuedIntegration) GetIntegration(ctx context.Context, p, i string) (*persistence.AgentIntegrationApproval, error) {
	row, err := r.AgentGrantRepository.GetIntegration(ctx, p, i)
	if r.afterGet != nil {
		f := r.afterGet
		r.afterGet = nil
		f()
	}
	return row, err
}

// #119 real REST worker routes must fence after header/query/empty auth assembly.
func TestQueuedActionREST(t *testing.T) {
	for _, auth := range []string{"header", "query", "none"} {
		for _, when := range []string{"queued", "resolving", "after-final", "daemon"} {
			t.Run(auth+"/"+when, func(t *testing.T) {
				f := newAgentAdminFixture(t)
				ctx := context.Background()
				var receivedMu sync.Mutex
				received := []string{}
				var rotate func()
				var rotateInHandler func() error
				handlerErr := make(chan error, 1)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					value := r.Header.Get("Authorization")
					if auth == "query" {
						value = r.URL.Query().Get("key")
					}
					receivedMu.Lock()
					received = append(received, value)
					receivedMu.Unlock()
					if when == "after-final" {
						if e := rotateInHandler(); e != nil {
							handlerErr <- e
							w.WriteHeader(500)
							return
						}
					}
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()
				f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "REST"})
				ai := agentadmin.APIAuthInput{}
				if auth != "none" {
					ai.Credential = "MAIL_TOKEN"
					ai.Prefix = "Bearer "
				}
				f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "comms", Name: "pay", BaseURL: srv.URL, Methods: []string{"POST"}, Writes: true, Auth: ai}))
				p := f.c.Registry.GetProject("hermes--comms")
				if auth == "query" {
					p.APIs[0].Auth.QueryParam = "key"
					p.APIs[0].Auth.Prefix = ""
				}
				st, err := f.c.secretStoreForWrite()
				if err != nil {
					t.Fatal(err)
				}
				publishErr := func(value, scope string) error {
					row, e := st.PreparePut("hermes", "MAIL_TOKEN", "secret", []byte(value), "device")
					if e != nil {
						return e
					}
					ticket, e := f.c.repos.AccountTransitions.Begin(ctx, scope, "fixture", "rotation", time.Now())
					if e != nil {
						return e
					}
					if e = f.c.repos.AccountTransitions.Finish(ctx, ticket, persistence.AccountPublication{Secret: row}); e != nil {
						return e
					}
					return nil
				}
				publish := func(value, scope string) {
					if e := publishErr(value, scope); e != nil {
						t.Fatal(e)
					}
				}
				rotateInHandler = func() error { return publishErr("Carol", "ns:hermes") }
				publish("Bob", "ns:hermes")
				rotate = func() {
					scope := "ns:hermes"
					if when == "daemon" {
						scope = persistence.AccountDaemonScope
					}
					publish("Carol", scope)
				}
				if when == "resolving" {
					if auth == "none" {
						f.c.repos.AgentGrants = &queuedIntegration{AgentGrantRepository: f.c.repos.AgentGrants, afterGet: rotate}
					} else {
						key, e := secretstore.LoadKey(f.c.storeKeyPath())
						if e != nil {
							t.Fatal(e)
						}
						wrapped, e := secretstore.New(&queuedSecrets{AgentSecretRepository: f.c.repos.AgentSecrets, afterGet: rotate}, key)
						if e != nil {
							t.Fatal(e)
						}
						f.c.setSecretStore(wrapped)
					}
				}
				args := []byte(`{"amount":1}`)
				sum, _ := approval.CanonicalSHA256(args)
				now := time.Now()
				a := &persistence.BrokerAction{ActionID: "rest", ProjectID: p.ID, TaskID: "task", WorkflowID: "pay", ActionKind: "pay", Tool: "api:pay:POST:/payment", Status: persistence.BrokerActionStaged, ArgsJSON: args, ArgsSHA256: sum, ExpiresAt: now.Add(time.Hour)}
				if _, err = f.c.repos.BrokerActions.Stage(ctx, a); err != nil {
					t.Fatal(err)
				}
				if _, err = f.c.repos.BrokerActions.PromoteStaged(ctx, a.TaskID); err != nil {
					t.Fatal(err)
				}
				if err = f.c.repos.BrokerActions.Approve(ctx, a.ActionID, sum, "device", now); err != nil {
					t.Fatal(err)
				}
				if when == "queued" || when == "daemon" {
					rotate()
				}
				w := brokeractions.New(brokeractions.Config{Repo: f.c.repos.BrokerActions, Caller: actionCaller{c: f.c}, WritesOn: func() bool { return true }, Logger: zerolog.Nop()})
				w.Execute(ctx, a.ActionID)
				got, e := f.c.repos.BrokerActions.Get(ctx, a.ActionID)
				if e != nil {
					t.Fatal(e)
				}
				select {
				case e := <-handlerErr:
					t.Fatal(e)
				default:
				}
				receivedMu.Lock()
				defer receivedMu.Unlock()
				if when == "after-final" {
					want := "Bearer Bob"
					if auth == "query" {
						want = "Bob"
					}
					if auth == "none" {
						want = ""
					}
					if len(received) != 1 || received[0] != want || got.Status != persistence.BrokerActionExecuted {
						t.Fatalf("old request %v %s/%s", received, got.Status, got.OutcomeClass)
					}
				} else if len(received) != 0 || got.Status != persistence.BrokerActionFailed || got.OutcomeClass != persistence.BrokerOutcomePreSendError {
					t.Fatalf("stale REST request %v %s/%s", received, got.Status, got.OutcomeClass)
				}
			})
		}
	}
}

// #119 phone must not name new-account metadata for an old proposal; its effect
// refusal is permanent even when a previously rendered phone page is replayed.
func TestQueuedActionPhone(t *testing.T) {
	for _, when := range []string{"before", "during"} {
		t.Run(when, func(t *testing.T) {
			f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
			ctx := context.Background()
			stageWrite(t, f, "mail", "phone", `{"to":"a@example.test","body":"hello"}`)
			a, err := f.c.repos.BrokerActions.Get(ctx, "phone")
			if err != nil {
				t.Fatal(err)
			}
			request := actionRequest(t, f, a.ActionID)
			rotate := func() {
				ticket, e := f.c.repos.AccountTransitions.Begin(ctx, "ns:hermes", "fixture", "rotation", time.Now())
				if e != nil {
					t.Fatal(e)
				}
				if e = f.c.repos.AccountTransitions.Finish(ctx, ticket, persistence.AccountPublication{}); e != nil {
					t.Fatal(e)
				}
			}
			if when == "before" {
				rotate()
			} else {
				f.c.repos.AgentGrants = &queuedIntegration{AgentGrantRepository: f.c.repos.AgentGrants, afterGet: rotate}
			}
			if _, err = f.svc.actionApprovalRow(ctx, "hermes", a); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatalf("stale phone render=%v", err)
			}
			if err = f.svc.actionEffect(ctx, *request); !errors.Is(err, approverdevice.ErrPermanent) {
				t.Fatalf("stale phone effect=%v", err)
			}
		})
	}
}
