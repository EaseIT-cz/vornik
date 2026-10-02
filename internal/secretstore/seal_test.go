package secretstore

import (
	"bytes"
	"strings"
	"testing"
)

// Plan P4.4: a sealed value opens only for its namespace and label, and
// carries no plaintext. Control: Seal/Open's additional data.
func TestSealOpen(t *testing.T) {
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{1}, 32))
	sealed, err := st.Seal("hermes", "OAUTH_bank/access", []byte("at-secret"))
	if err != nil || !strings.HasPrefix(sealed, SealedPrefix) || strings.Contains(sealed, "at-secret") {
		t.Fatalf("seal: %q %v", sealed, err)
	}
	if got, err := st.Open("hermes", "OAUTH_bank/access", sealed); err != nil || string(got) != "at-secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	for name, open := range map[string]func() ([]byte, error){
		"other namespace": func() ([]byte, error) { return st.Open("codex", "OAUTH_bank/access", sealed) },
		"other label":     func() ([]byte, error) { return st.Open("hermes", "OAUTH_bank/refresh", sealed) },
		"other server":    func() ([]byte, error) { return st.Open("hermes", "OAUTH_mail/access", sealed) },
		"plaintext":       func() ([]byte, error) { return st.Open("hermes", "OAUTH_bank/access", "at-secret") },
		"label w/o slash": func() ([]byte, error) { return st.Open("hermes", "FIO", sealed) },
	} {
		if _, err := open(); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	again, _ := st.Seal("hermes", "OAUTH_bank/access", []byte("at-secret"))
	if again == sealed {
		t.Fatal("two seals of one value are equal; the swap compares sealed values")
	}
	if s, err := st.Seal("hermes", "OAUTH_bank/refresh", nil); err != nil || s != "" {
		t.Fatalf("an empty value must stay empty: %q %v", s, err)
	}
}
