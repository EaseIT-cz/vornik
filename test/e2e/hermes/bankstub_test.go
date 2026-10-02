package hermes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/secrets"
)

// Agent-administered Vornik plan P8.1: the bank stub serves transactions
// whose descriptions carry the raw-record canary, only to a request with the
// bank key; it records each request and whether it was authorised. Control:
// BankStub.ServeHTTP.
func TestBankStub(t *testing.T) {
	bank := NewBankStub()
	srv := httptest.NewServer(bank)
	defer srv.Close()
	get := func(path, key string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/v1/accounts/main/transactions?from=2026-09", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	code, body := get("/v1/accounts/main/transactions?from=2026-09", BankKeyCanary)
	if code != http.StatusOK || !strings.Contains(body, RawRecordCanary) {
		t.Fatalf("with key: %d %s", code, body)
	}
	var out struct {
		Transactions []struct {
			Amount   float64 `json:"amount"`
			Category string  `json:"category"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Transactions) < 3 {
		t.Fatalf("body: %v %s", err, body)
	}
	if code, _ := get("/v1/elsewhere", BankKeyCanary); code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", code)
	}
	reqs := bank.Requests()
	if len(reqs) != 3 || reqs[0].Authorized || !reqs[1].Authorized {
		t.Fatalf("requests: %+v", reqs)
	}
}

// Review 20261002-69cc R2: the bank key canary has a canonical credential
// shape the egress scan detects, so the lane's egress_secret arm really
// exercises the scan. Control: the canary's shape against internal/secrets.
func TestBankKeyCanaryIsDetected(t *testing.T) {
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := egressscan.ScanJSON(det, []byte(`{"note":"`+BankKeyCanary+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, blocks := egressscan.Blocking(fs); !blocks {
		t.Fatalf("the bank canary is not credential-shaped to the scanner: %+v", fs)
	}
	if !strings.Contains(BankKeyCanary, "CANARYBANK") {
		t.Fatal("the canary lost its marker")
	}
}

// The agent's mail server requires its token, offers a read and a write
// tool, and puts the raw-mail canary only in message bodies. Control:
// agentMailStub.
func TestAgentMailStub(t *testing.T) {
	stub := agentMailStub()
	srv := httptest.NewServer(stub)
	defer srv.Close()
	if code := func() int {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}(); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mail_search","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer "+MailTokenCanary)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), RawMailCanary) || len(stub.Calls()) != 1 {
		t.Fatalf("mail_search: %s", b)
	}
}
