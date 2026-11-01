package secretstore

import (
	"bytes"
	"errors"
	"testing"
)

// GitHub #571 A1: the operator domain is its own key, which no agent
// namespace shares, and a value opens only under the label it was sealed for.
func TestOperatorDomain571(t *testing.T) {
	s, err := New(nil, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.SealOperator("OAUTH/p/jira/access", []byte("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OpenOperator("OAUTH/p/jira/access", sealed); err != nil || string(got) != "tok" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s.OpenOperator("OAUTH/q/jira/access", sealed); err == nil {
		t.Error("opened under another label")
	}
	// No namespace opens it, and the namespace path refuses the domain name.
	if _, err := s.Open("operator", "OAUTH/p/jira/access", sealed); err == nil {
		t.Error("an agent namespace opened an operator value")
	}
	if _, err := s.Seal(operatorDomain, "OAUTH/p/jira/access", []byte("x")); err == nil {
		t.Error("Seal accepted the operator domain as a namespace")
	}
	if _, err := s.OpenOperator("OAUTH/p/jira/access", "plain"); !errors.Is(err, ErrNotSealed) {
		t.Errorf("an unsealed value: %v", err)
	}
	if v, err := s.SealOperator("OAUTH/p/jira/access", nil); err != nil || v != "" {
		t.Errorf("an empty value: %q %v", v, err)
	}
	if _, err := s.SealOperator("no-slash", []byte("x")); err == nil {
		t.Error("a label without / was accepted")
	}
}
