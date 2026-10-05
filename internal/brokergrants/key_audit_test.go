package brokergrants

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestKeyOf_AuditExactIntegers(t *testing.T) {
	a, err := KeyOf([]string{"id"}, nil, []byte(`{"id":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := KeyOf([]string{"id"}, nil, []byte(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash {
		t.Fatalf("distinct standing grant IDs share key: %s", a.Canonical)
	}
}

func TestKeyOf_AuditLegacyNumericGrants(t *testing.T) {
	for _, raw := range []string{`{"id":9007199254740992}`, `{"id":{"nested":[9007199254740992]}}`, `{"id":1.25}`, `{"id":1}`} {
		k, err := KeyOf([]string{"id"}, nil, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		legacy := sha256.Sum256(k.Canonical)
		if k.Hash == hex.EncodeToString(legacy[:]) {
			t.Fatalf("numeric key still matches potentially lossy legacy grant: %s", raw)
		}
	}
	k, err := KeyOf([]string{"id"}, nil, []byte(`{"id":"record-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	legacy := sha256.Sum256(k.Canonical)
	if k.Hash != hex.EncodeToString(legacy[:]) {
		t.Fatal("non-numeric grants should remain compatible")
	}
}

func TestKeyOf_AuditCompositeNumbers(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"id":{"nested":9007199254740992}}`, `{"id":{"nested":9007199254740993}}`},
		{`{"id":[9007199254740992]}`, `{"id":[9007199254740993]}`},
	} {
		a, err := KeyOf([]string{"id"}, nil, []byte(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := KeyOf([]string{"id"}, nil, []byte(pair[1]))
		if err != nil {
			t.Fatal(err)
		}
		if a.Hash == b.Hash {
			t.Fatal("nested numeric identities collided")
		}
	}
}

func TestKeyFromCanonical_AuditExactDisplay(t *testing.T) {
	k, err := keyFromCanonical([]string{"id"}, []byte(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Display(); got != "id 9007199254740993" {
		t.Fatalf("grant page changed the approved key: %s", got)
	}
}
