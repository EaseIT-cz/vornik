package agentns

import "testing"

func TestValid(t *testing.T) {
	for _, ns := range []string{"hermes", "ab", "codex2", "a1234567890bcdef"} {
		if !Valid(ns) {
			t.Errorf("Valid(%q) = false, want true", ns)
		}
	}
	for _, ns := range []string{"", "a", "Hermes", "2codex", "a-b", "a--b", "a_b", "abcdefghijklmnopq", "ns/x"} {
		if Valid(ns) {
			t.Errorf("Valid(%q) = true, want false", ns)
		}
	}
}

func TestFromID(t *testing.T) {
	cases := []struct {
		id     string
		wantNS string
		wantOK bool
	}{
		{"hermes--finance", "hermes", true},
		{"hermes--finance--q1", "hermes", true}, // the namespace is the part before the FIRST separator
		{"assistant", "", false},
		{"my-project", "", false},
		{"Hermes--finance", "", false}, // an invalid namespace is not a namespace
		{"--finance", "", false},
		{"hermes--", "", false},
	}
	for _, c := range cases {
		ns, ok := FromID(c.id)
		if ns != c.wantNS || ok != c.wantOK {
			t.Errorf("FromID(%q) = (%q, %v), want (%q, %v)", c.id, ns, ok, c.wantNS, c.wantOK)
		}
	}
}

func TestID(t *testing.T) {
	if got := ID("hermes", "finance"); got != "hermes--finance" {
		t.Fatalf("ID = %q", got)
	}
	if ns, ok := FromID(ID("hermes", "finance")); !ok || ns != "hermes" {
		t.Fatalf("FromID(ID(...)) = (%q, %v)", ns, ok)
	}
}

func TestValidSecretName(t *testing.T) {
	for _, ok := range []string{"A", "FIO_TOKEN", "X9"} {
		if !ValidSecretName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "lower", "9A", "A-B", "A.B", "A/B"} {
		if ValidSecretName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
