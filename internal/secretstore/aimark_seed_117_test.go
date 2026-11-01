package secretstore

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"
)

// GitHub #117 (Art 50 design §14.7): the AI-mark signing seed is derived
// from the master key, in its own HKDF domain.

func TestAIMarkSigningSeed_SameMasterSameSeed_117(t *testing.T) {
	m := bytes.Repeat([]byte{7}, 32)
	a, err := newStore(t, newMemRepo(), m).AIMarkSigningSeed()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newStore(t, newMemRepo(), m).AIMarkSigningSeed()
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatal("two daemons sharing store.key must derive one signing seed")
	}
	c, _ := newStore(t, newMemRepo(), bytes.Repeat([]byte{8}, 32)).AIMarkSigningSeed()
	if bytes.Equal(a, c) {
		t.Fatal("a different master key must derive a different seed")
	}
}

// Domain separation (review 3701 F1): the seed is not any key the store
// derives for encryption, whatever the namespace.
func TestAIMarkSigningSeed_SeparateFromEncryptionKeys_117(t *testing.T) {
	m := bytes.Repeat([]byte{7}, 32)
	seed, _ := newStore(t, newMemRepo(), m).AIMarkSigningSeed()
	for _, info := range []string{aimarkSigningInfo, operatorDomain, "agent1"} {
		k, err := hkdf.Key(sha256.New, m, []byte(hkdfSalt), info, keySize)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(seed, k) {
			t.Fatalf("the signing seed equals the encryption key for info %q", info)
		}
	}
}
