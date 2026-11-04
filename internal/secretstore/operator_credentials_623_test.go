package secretstore

// GitHub #623 (found at: production, 2026-11-02): an operator, or a managed
// instance's tenant_admin with no shell, could not enter a credential that a
// daemon-level configuration refers to (secret://NAME): such a name was read
// only from the process environment. Operator credentials are now rows in a
// reserved namespace no agent path can address, and an un-namespaced name
// reads the environment first, then those rows. Design:
// 2026-10-02-agent-administered-vornik-design.md, Amendment 2026-11-02 (#623).

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/mcpauth"
)

func TestOperatorCredential623_RoundTripListDelete(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	st := newStore(t, repo, bytes.Repeat([]byte{7}, 32))

	if err := st.PutOperatorCredential(ctx, "CONTEXT7_API_KEY", []byte("ctx7-secret-value"), "console:user_1"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := st.GetOperatorCredential(ctx, "CONTEXT7_API_KEY")
	if err != nil || string(got) != "ctx7-secret-value" {
		t.Fatalf("get = (%q, %v)", got, err)
	}
	for _, r := range repo.rows {
		if bytes.Contains(r.Ciphertext, []byte("ctx7-secret-value")) {
			t.Fatal("the stored row holds the plaintext")
		}
		if r.CreatedByDevice != "console:user_1" {
			t.Errorf("created_by = %q, want the console principal", r.CreatedByDevice)
		}
	}
	metas, err := st.ListOperatorCredentials(ctx)
	if err != nil || len(metas) != 1 || metas[0].Name != "CONTEXT7_API_KEY" {
		t.Fatalf("list = (%+v, %v)", metas, err)
	}
	if err := st.DeleteOperatorCredential(ctx, "CONTEXT7_API_KEY"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetOperatorCredential(ctx, "CONTEXT7_API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestOperatorCredential623_RefusesBadInput(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{7}, 32))
	for _, name := range []string{"context7-mcp", "", "lower", "A/B", "1ABC"} {
		if err := st.PutOperatorCredential(ctx, name, []byte("v"), "console:u"); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	if err := st.PutOperatorCredential(ctx, "EMPTY_VALUE", nil, "console:u"); err == nil {
		t.Error("an empty value was accepted")
	}
}

// No agent path reaches the reserved namespace: the agent functions refuse it
// by name, and a row stored there does not open under another namespace.
func TestOperatorCredential623_AgentPathsCannotAddressIt(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{7}, 32))
	if err := st.PutOperatorCredential(ctx, "TOKEN", []byte("v"), "console:u"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, operatorCredentialsNamespace, "TOKEN"); err == nil {
		t.Error("the agent Get read an operator credential")
	}
	if err := st.Put(ctx, operatorCredentialsNamespace, "TOKEN", "secret", []byte("x"), "d"); err == nil {
		t.Error("the agent Put wrote into the operator namespace")
	}
	if _, err := st.List(ctx, operatorCredentialsNamespace); err == nil {
		t.Error("the agent List listed the operator namespace")
	}
	if err := st.Delete(ctx, operatorCredentialsNamespace, "TOKEN"); err == nil {
		t.Error("the agent Delete removed an operator credential")
	}
	src := Source{Store: st, Env: envMap{}}
	if _, ok := src.Get(operatorCredentialsNamespace + "/TOKEN"); ok {
		t.Error("secret://_operator_credentials/TOKEN resolved")
	}
}

func TestOperatorCredential623_SourceReadsEnvironmentFirstThenStore(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{7}, 32))
	_ = st.PutOperatorCredential(ctx, "ONLY_IN_STORE", []byte("store-value"), "console:u")
	_ = st.PutOperatorCredential(ctx, "IN_BOTH", []byte("store-value"), "console:u")
	src := Source{Store: st, Env: envMap{"IN_BOTH": "env-value", "ONLY_IN_ENV": "env-only"}}

	cases := []struct {
		name, want string
		ok         bool
	}{
		{"ONLY_IN_STORE", "store-value", true},
		{"IN_BOTH", "env-value", true},
		{"ONLY_IN_ENV", "env-only", true},
		{"NOWHERE", "", false},
		{"context7-mcp", "", false},
	}
	for _, c := range cases {
		v, ok := src.Get(c.name)
		if ok != c.ok || v != c.want {
			t.Errorf("Get(%q) = (%q, %v), want (%q, %v)", c.name, v, ok, c.want, c.ok)
		}
	}
	// No store: the environment alone, as before.
	if _, ok := (Source{Env: envMap{}}).Get("ONLY_IN_STORE"); ok {
		t.Error("resolved from a store that is not there")
	}
}

// Change review F4 (review-20261102-9528): what keeps an agent project off the
// operator credentials is mcpauth.CheckNamespace inside Resolve, not
// Source.Get, whose fallback is namespace-agnostic. An agent project's
// un-namespaced reference is refused even when an operator credential of that
// name exists; the same reference from a daemon-level server resolves.
func TestOperatorCredential623_AgentProjectNeverReadsAnOperatorCredential(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{7}, 32))
	if err := st.PutOperatorCredential(ctx, "FOO", []byte("operator-only-value"), "console:u"); err != nil {
		t.Fatal(err)
	}
	src := Source{Store: st, Env: envMap{}}
	auth := mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://FOO", ValuePrefix: "Bearer "}

	inj, err := mcpauth.Resolve(auth, "streamable-http", src, mcpauth.Grants{Namespace: "hermes", Allowed: []string{"FOO", "hermes/FOO"}})
	if err == nil {
		t.Fatalf("an agent project resolved an operator credential: %+v", inj)
	}
	if strings.Contains(err.Error(), "operator-only-value") {
		t.Fatalf("the refusal carries the value: %v", err)
	}

	inj, err = mcpauth.Resolve(auth, "streamable-http", src, mcpauth.Grants{Unrestricted: true})
	if err != nil {
		t.Fatalf("a daemon-level server could not resolve the operator credential: %v", err)
	}
	if got := inj.Headers["Authorization"]; got != "Bearer operator-only-value" {
		t.Fatalf("Authorization = %q", got)
	}
}

// Change review F3: an empty environment, a key error and a valid name report
// the key error and miss, before any store call.
func TestOperatorCredential623_KeyErrorIsReportedForAnUnNamespacedMiss(t *testing.T) {
	var reported []string
	src := Source{Env: envMap{}, KeyErr: errors.New("store key unusable"),
		OnError: func(name string, _ error) { reported = append(reported, name) }}
	if _, ok := src.Get("CONTEXT7_API_KEY"); ok {
		t.Fatal("resolved with an unusable key")
	}
	if len(reported) != 1 || reported[0] != "CONTEXT7_API_KEY" {
		t.Fatalf("reported = %v, want the key error for CONTEXT7_API_KEY", reported)
	}
	// An invalid name is a plain environment miss: no store, no report.
	reported = nil
	if _, ok := src.Get("context7-mcp"); ok || len(reported) != 0 {
		t.Fatalf("invalid name: ok=%v reported=%v", ok, reported)
	}
	// The environment still wins with an unusable key.
	src.Env = envMap{"CONTEXT7_API_KEY": "env"}
	if v, ok := src.Get("CONTEXT7_API_KEY"); !ok || v != "env" {
		t.Fatalf("env value lost: (%q, %v)", v, ok)
	}
}
