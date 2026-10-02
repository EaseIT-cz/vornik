package hermes

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// The bank stub for agent-administered Vornik's DoD lane (plan P8.1): a REST
// bank the agent connects with add_api, whose key the phone enters.

// BankKeyCanary is the bank API key the phone enters. It has the shape of a
// credential the egress scanner detects (openai_key, sk- plus 32 or more
// alphanumerics), so a hostile producer that echoes it is refused as
// egress_secret (review 20261002-69cc R2).
const BankKeyCanary = "sk-CANARYBANK0123456789abcdefghijklmnopqrstu"

// RawRecordCanary marks every raw transaction line: it must never reach a
// harness, only the approved summary may.
const RawRecordCanary = "RAWTXN-CANARY-7f3c"

// BankRequest is one recorded request.
type BankRequest struct {
	Path       string
	Authorized bool
}

// BankStub serves GET /v1/accounts/{id}/transactions?from=YYYY-MM.
type BankStub struct {
	mu   sync.Mutex
	reqs []BankRequest
}

// NewBankStub builds a bank stub.
func NewBankStub() *BankStub { return &BankStub{} }

// Requests returns the recorded requests, in order.
func (b *BankStub) Requests() []BankRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BankRequest(nil), b.reqs...)
}

// ServeHTTP implements the bank API.
func (b *BankStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authorized := r.Header.Get("Authorization") == "Bearer "+BankKeyCanary
	b.mu.Lock()
	b.reqs = append(b.reqs, BankRequest{Path: r.URL.Path, Authorized: authorized})
	b.mu.Unlock()
	if !authorized {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method != http.MethodGet || len(parts) != 4 || parts[0] != "v1" || parts[1] != "accounts" || parts[3] != "transactions" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	month := r.URL.Query().Get("from")
	type txn struct {
		Date        string  `json:"date"`
		Amount      float64 `json:"amount"`
		Category    string  `json:"category"`
		Description string  `json:"description"`
	}
	out := []txn{
		{month + "-03", -42.10, "groceries", "Grocer 1101 " + RawRecordCanary + " card *4411"},
		{month + "-09", -1200, "rent", "Landlord s.r.o. " + RawRecordCanary + " IBAN CZ65 0800 0000 1920 0014 5399"},
		{month + "-15", -18.99, "subscriptions", "Streaming " + RawRecordCanary},
		{month + "-21", -64.30, "groceries", "Grocer 1101 " + RawRecordCanary},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"account": parts[2], "transactions": out})
}

// MailTokenCanary is the agent mail server's static token, which the phone
// enters. It has the github_pat shape, so a leak is also credential-shaped.
const MailTokenCanary = "ghp_CANARYMAIL0123456789abcdefghijklmnopq"

// RawMailCanary marks the agent mail stub's message bodies.
const RawMailCanary = "RAWMAIL-CANARY-91be"

// agentMailStub is the mail server the agent connects with add_mcp_server:
// mail_search (read) returns a message whose body carries RawMailCanary;
// drafts_create (write) records the drafts it is asked to make. It requires
// MailTokenCanary.
func agentMailStub() *MCPStub {
	s := NewMCPStub("agent-mail",
		MCPTool{Name: "mail_search", Description: "Search the mailbox.", Handle: func(json.RawMessage) (string, bool) {
			return `{"messages":[{"id":"m-7","from":"landlord@flat.example","subject":"Rent","body":"Please confirm the rent for October. ` + RawMailCanary + `"}]}`, false
		}},
		MCPTool{Name: "drafts_create", Description: "Create a draft reply.", Handle: func(json.RawMessage) (string, bool) {
			return `{"draft_id":"d-1"}`, false
		}},
	)
	s.Token = MailTokenCanary
	return s
}
