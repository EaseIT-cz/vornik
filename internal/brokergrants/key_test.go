package brokergrants

import (
	"testing"
)

// Broker write-actions design, "Tier 2, revised" item 3 and round 3
// (normalisation completed): the local part is kept as written (case and
// plus-tags significant), the domain is lower-cased, display names and
// surrounding space are dropped.
func TestKeyOf_NormalisesDestinations(t *testing.T) {
	paths := []string{"to"}
	dest := map[string]bool{"to": true}
	same := []string{
		`{"to":"jana@Example.COM","body":"a"}`,
		`{"to":"  jana@example.com ","body":"b"}`,
		`{"to":"Jana Novak <jana@example.com>","body":"c"}`,
	}
	var want string
	for i, args := range same {
		k, err := KeyOf(paths, dest, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if i == 0 {
			want = k.Hash
			continue
		}
		if k.Hash != want {
			t.Errorf("%s hashed %s, want %s (the same normalised key)", args, k.Hash, want)
		}
	}
	for _, other := range []string{
		`{"to":"Jana@example.com"}`,      // local part case is significant
		`{"to":"jana+news@example.com"}`, // a plus-tag is another address
		`{"to":"jana@example.org"}`,
	} {
		k, err := KeyOf(paths, dest, []byte(other))
		if err != nil {
			t.Fatal(err)
		}
		if k.Hash == want {
			t.Errorf("%s matched jana@example.com; it is another address", other)
		}
	}
}

// Round 4 (a3e2 F1): a property test that the hash is a pure function of the
// normalised key, and that distinct normalised keys never collide, over a
// corpus of case, plus-tag, display-name and cc/bcc variants.
func TestKeyOf_HashIsPureAndDistinctKeysNeverCollide(t *testing.T) {
	paths := []string{"to", "cc", "bcc"}
	dest := map[string]bool{"to": true, "cc": true, "bcc": true}
	corpus := []string{
		`{"to":"a@x.com"}`,
		`{"to":"A@x.com"}`,
		`{"to":"a+t@x.com"}`,
		`{"to":"a@x.org"}`,
		`{"to":"a@x.com","cc":["b@x.com"]}`,
		`{"to":"a@x.com","cc":["b@x.com","c@x.com"]}`,
		`{"to":"a@x.com","bcc":["b@x.com"]}`,
		`{"to":"a@x.com","cc":[]}`,
		`{"to":"a@x.com","cc":["B@x.com"]}`,
		`{"to":"a@x.com","cc":"b@x.com"}`,
		`{"cc":["a@x.com"]}`,
	}
	byHash := map[string]string{}
	for _, args := range corpus {
		k1, err := KeyOf(paths, dest, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		k2, _ := KeyOf([]string{"bcc", "to", "cc"}, dest, []byte(args))
		if k1.Hash != k2.Hash || string(k1.Canonical) != string(k2.Canonical) {
			t.Errorf("%s: the hash depends on the order the paths are listed in", args)
		}
		if prev, dup := byHash[k1.Hash]; dup {
			t.Errorf("%s and %s collide", prev, args)
		}
		byHash[k1.Hash] = args
	}
	// Display names and order inside a destination list do not change the key.
	a, _ := KeyOf(paths, dest, []byte(`{"to":"a@x.com","cc":["c@x.com","Bee <b@X.com>"]}`))
	b, _ := KeyOf(paths, dest, []byte(`{"to":"a@x.com","cc":["b@x.com","c@x.com"]}`))
	if a.Hash != b.Hash {
		t.Error("a cc list differing only in order and display names hashed differently")
	}
}

// A non-destination key path (a calendar answer's response) is compared
// exactly; a body never enters the key.
func TestKeyOf_NonDestinationPathsAreExact(t *testing.T) {
	paths := []string{"organizer_domain", "response"}
	dest := map[string]bool{"organizer_domain": true}
	a, _ := KeyOf(paths, dest, []byte(`{"organizer_domain":"x.com","response":"accept","note":"1"}`))
	b, _ := KeyOf(paths, dest, []byte(`{"organizer_domain":"x.com","response":"accept","note":"2"}`))
	c, _ := KeyOf(paths, dest, []byte(`{"organizer_domain":"x.com","response":"decline"}`))
	if a.Hash != b.Hash {
		t.Error("an argument outside the key changed the key")
	}
	if a.Hash == c.Hash {
		t.Error("a different response matched")
	}
}

func TestKeyOf_RefusesNonObjectArgs(t *testing.T) {
	if _, err := KeyOf([]string{"to"}, nil, []byte(`[1]`)); err == nil {
		t.Error("an array was accepted as arguments")
	}
	if _, err := KeyOf(nil, nil, []byte(`{}`)); err == nil {
		t.Error("an empty key was accepted")
	}
}

// Display is what the grant page and the offer show: the normalised values.
func TestKey_Display(t *testing.T) {
	k, err := KeyOf([]string{"to", "cc"}, map[string]bool{"to": true, "cc": true}, []byte(`{"to":"Jana <jana@Example.com>","cc":["b@x.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Display(); got != "to jana@example.com, cc b@x.com" {
		t.Errorf("Display = %q", got)
	}
}
