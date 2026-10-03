// Package brokergrants is tier 2 of the broker write-actions design's
// approval-fatigue proposal: standing grants
// (https://docs.vornik.io,
// "Tier 2: standing grants" as revised by rounds 3 and 4 and review 61a5).
//
// A standing grant lets later writes of one declared class execute without a
// per-write approval, within a key, a count and an expiry a person set when
// approving a seed write. The bytes of a covered write are the agent's and
// are not shown to anyone before they are sent: that is the concession, and
// the reason for the bounds, the digest, pause and revoke.
package brokergrants

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"vornik.io/vornik/internal/approval"
)

// Key is an action's normalised standing key: the key paths' values,
// destinations normalised. Canonical is its canonical JSON and Hash the
// SHA-256 of that, the grant row's key_hash.
type Key struct {
	Paths     []string
	Values    map[string]any
	Canonical []byte
	Hash      string
}

// KeyOf is THE canonicalisation of a standing key (review 61a5 F1): grant
// creation and the guarded decrement both call it, and a source test pins
// that no second copy exists. paths are the proposal's standing.key,
// destinations the args_schema's x-destination arguments, args the action's
// canonical argument bytes. An absent path is null in the key.
func KeyOf(paths []string, destinations map[string]bool, args []byte) (Key, error) {
	if len(paths) == 0 {
		return Key{}, errors.New("brokergrants: a standing key names at least one argument")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil || obj == nil {
		return Key{}, errors.New("brokergrants: the arguments are not a JSON object")
	}
	values := make(map[string]any, len(paths))
	for _, p := range paths {
		raw, ok := obj[p]
		if !ok {
			values[p] = nil
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return Key{}, fmt.Errorf("brokergrants: argument %q: %w", p, err)
		}
		if destinations[p] {
			var err error
			if v, err = normaliseDestination(v); err != nil {
				return Key{}, fmt.Errorf("brokergrants: argument %q: %w", p, err)
			}
		}
		values[p] = v
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return Key{}, err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return Key{}, err
	}
	sum := sha256.Sum256(canon)
	return Key{Paths: append([]string(nil), paths...), Values: values, Canonical: canon, Hash: hex.EncodeToString(sum[:])}, nil
}

// keyFromCanonical rebuilds a Key for display from an opened grant's
// canonical JSON. It never computes a hash a decrement would use.
func keyFromCanonical(paths []string, canonical []byte) (Key, error) {
	var values map[string]any
	if err := json.Unmarshal(canonical, &values); err != nil {
		return Key{}, err
	}
	return Key{Paths: paths, Values: values, Canonical: canonical}, nil
}

// errNotAddress: a destination value that is not address-shaped.
var errNotAddress = errors.New("a destination must be an address or a list of addresses")

// normaliseDestination normalises one destination value: a string address,
// or a list of them (sorted, since a recipient list is a set). null is an
// absent destination. Anything else is refused (review f819): a destination
// is address-shaped, so it is never compared in order or as raw JSON.
func normaliseDestination(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return NormaliseAddress(t), nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, errNotAddress
			}
			out[i] = NormaliseAddress(s)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
		return out, nil
	}
	return nil, errNotAddress
}

// NormaliseAddress normalises an address: surrounding space and a display name dropped, the domain
// lower-cased, the local part kept as written (case and plus-tags are
// significant: a different tag is a different address). A value that is not
// an address is only trimmed.
func NormaliseAddress(s string) string {
	s = strings.TrimSpace(s)
	if a, err := mail.ParseAddress(s); err == nil {
		s = a.Address
	}
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return s
	}
	return s[:at] + "@" + strings.ToLower(s[at+1:])
}

// Display renders the key for a person: "to a@x.com, cc b@x.com", in the
// declared path order. Shown only on the device and /inbox pages.
func (k Key) Display() string {
	parts := make([]string, 0, len(k.Paths))
	for _, p := range k.Paths {
		parts = append(parts, p+" "+displayValue(k.Values[p]))
	}
	return strings.Join(parts, ", ")
}

func displayValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "(none)"
	case string:
		return t
	case []any:
		if len(t) == 0 {
			return "(none)"
		}
		s := make([]string, len(t))
		for i, e := range t {
			s[i] = displayValue(e)
		}
		return strings.Join(s, " and ")
	}
	b, _ := json.Marshal(v)
	return string(b)
}
