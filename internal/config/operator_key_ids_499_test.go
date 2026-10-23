package config

import (
	"strings"
	"testing"
)

// GitHub #499 (operator decision A19): admin.operator_key_ids names the
// operator's own static keys by hash id; the strict loader refuses an id that
// is malformed or names no allowed key, and only a listed key is the
// operator's.
func TestAdminOperatorKeyIDs(t *testing.T) {
	a := AdminConfig{Enabled: true, AllowedKeys: []string{"sk-vornik-operator", "sk-vornik-broker"}}
	op := KeyHashID("sk-vornik-operator")
	if len(op) != 16 {
		t.Fatalf("id %q", op)
	}
	a.OperatorKeyIDs = []string{op}
	if err := a.ValidateOperatorKeyIDs(); err != nil {
		t.Fatalf("a listed operator key was refused: %v", err)
	}
	if !a.IsOperatorKeyID(op) || a.IsOperatorKeyID(KeyHashID("sk-vornik-broker")) {
		t.Fatal("only the listed key is the operator's")
	}
	for _, bad := range []string{"sk-vornik-operator", strings.ToUpper(op), op[:15], KeyHashID("sk-not-allowed")} {
		a.OperatorKeyIDs = []string{bad}
		if err := a.ValidateOperatorKeyIDs(); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	off := AdminConfig{OperatorKeyIDs: []string{op}}
	if off.IsOperatorKeyID(op) {
		t.Fatal("the admin surface is off: no key is the operator's")
	}
}
