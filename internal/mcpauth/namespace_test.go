package mcpauth

import (
	"strings"
	"testing"
)

// Agent-administered Vornik design §8.1: namespaced secret references.

func TestParseSecretRef_Namespaced(t *testing.T) {
	ok := map[string]string{
		"secret://hermes/FIO_TOKEN": "hermes/FIO_TOKEN",
		"secret://GITHUB_TOKEN":     "GITHUB_TOKEN",
		"secret://my.token-v2":      "my.token-v2", // Review Focus 1: operator names keep . and -
	}
	for in, want := range ok {
		got, valid := ParseSecretRef(in)
		if !valid || got != want {
			t.Errorf("ParseSecretRef(%q) = (%q, %v), want (%q, true)", in, got, valid, want)
		}
	}
	// Review Focus 4: malformed namespaced forms are refused, never split leniently.
	for _, in := range []string{
		"secret://Hermes/X", "secret://a--b/X", "secret://hermes/a/b", "secret://hermes/",
		"secret:///X", "secret://hermes/lower", "secret://hermes/X.Y",
	} {
		if got, valid := ParseSecretRef(in); valid {
			t.Errorf("ParseSecretRef(%q) = (%q, true), want refused", in, got)
		}
	}
}

func TestSplitSecretName(t *testing.T) {
	if ns, bare, n := SplitSecretName("hermes/FIO_TOKEN"); !n || ns != "hermes" || bare != "FIO_TOKEN" {
		t.Fatalf("got (%q, %q, %v)", ns, bare, n)
	}
	if _, bare, n := SplitSecretName("GITHUB_TOKEN"); n || bare != "GITHUB_TOKEN" {
		t.Fatalf("flat name reported namespaced")
	}
}

func TestCheckNamespace(t *testing.T) {
	// An agent project may reach only its own namespace.
	if err := CheckNamespace([]string{"hermes/A", "hermes/B"}, "hermes"); err != nil {
		t.Fatalf("own namespace refused: %v", err)
	}
	for _, refs := range [][]string{{"GITHUB_TOKEN"}, {"codex/A"}} {
		err := CheckNamespace(refs, "hermes")
		if err == nil {
			t.Fatalf("CheckNamespace(%v, hermes) = nil, want refusal", refs)
		}
		if !strings.Contains(err.Error(), refs[0]) {
			t.Fatalf("refusal does not name the reference: %v", err)
		}
	}
	// An operator project may not reach any agent namespace.
	if err := CheckNamespace([]string{"hermes/A"}, ""); err == nil {
		t.Fatal("operator project reached an agent namespace")
	}
	if err := CheckNamespace([]string{"GITHUB_TOKEN"}, ""); err != nil {
		t.Fatalf("operator flat ref refused: %v", err)
	}
}

func TestResolve_EnforcesNamespace(t *testing.T) {
	a := Auth{Mode: ModeStatic, ValueFrom: "secret://GITHUB_TOKEN"}
	_, err := Resolve(a, "streamable-http", mapSecrets{"GITHUB_TOKEN": "v"}, Grants{Allowed: []string{"GITHUB_TOKEN"}, Namespace: "hermes"})
	if err == nil {
		t.Fatal("agent project resolved a flat reference")
	}
	a = Auth{Mode: ModeStatic, ValueFrom: "secret://hermes/FIO"}
	inj, err := Resolve(a, "streamable-http", mapSecrets{"hermes/FIO": "v"}, Grants{Allowed: []string{"hermes/FIO"}, Namespace: "hermes"})
	if err != nil || inj.Headers["Authorization"] != "v" {
		t.Fatalf("own namespaced ref: %v %v", inj, err)
	}
}
