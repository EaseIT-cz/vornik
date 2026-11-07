package secretstore

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"
)

// GitHub #163 (agent-administered design part 09 §9.3a B.2): the Web Push
// VAPID seed is derived from the master key, in its own HKDF domain.

func TestApproverPushVAPIDSeed_SameMasterSameSeed_163(t *testing.T) {
	m := bytes.Repeat([]byte{7}, 32)
	a, err := newStore(t, newMemRepo(), m).ApproverPushVAPIDSeed()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newStore(t, newMemRepo(), m).ApproverPushVAPIDSeed()
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatal("two daemons sharing store.key must derive one VAPID seed")
	}
	c, _ := newStore(t, newMemRepo(), bytes.Repeat([]byte{8}, 32)).ApproverPushVAPIDSeed()
	if bytes.Equal(a, c) {
		t.Fatal("a different master key must derive a different seed")
	}
}

func TestApproverPushVAPIDSeed_SeparateFromOtherKeys_163(t *testing.T) {
	m := bytes.Repeat([]byte{7}, 32)
	st := newStore(t, newMemRepo(), m)
	seed, _ := st.ApproverPushVAPIDSeed()
	mark, _ := st.AIMarkSigningSeed()
	if bytes.Equal(seed, mark) {
		t.Fatal("the VAPID seed equals the AI-mark signing seed")
	}
	for _, info := range []string{approverPushInfo, operatorDomain, "agent1"} {
		k, err := hkdf.Key(sha256.New, m, []byte(hkdfSalt), info, keySize)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(seed, k) {
			t.Fatalf("the VAPID seed equals the encryption key for info %q", info)
		}
	}
}
