// Package approval is the one gate every human approval of a write passes
// through, and the one hash that binds an approval to what the approver saw.
//
// Broker write-actions design (https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md) §5.3. A safety check
// with two implementations has one that is wrong (CLAUDE.md §5): the
// supervised web-write approval, the broker-action approval and the daemon's
// CSRF middleware all use this package rather than carrying their own copy.
//
// The package has no dependencies inside the module, so both internal/api
// and internal/ui can import it.
package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Refusals, each mapped to one HTTP status by WriteError.
var (
	// ErrMethod: an approval is never reachable by a GET deep link.
	ErrMethod = errors.New("approval: method not allowed; approvals are POST only")
	// ErrCrossSite: no trustworthy same-origin signal.
	ErrCrossSite = errors.New("approval: cross-site or unverifiable origin")
	// ErrScope: the caller may not act on this project. Reported as 404 so a
	// scoped caller cannot probe for another tenant's rows.
	ErrScope = errors.New("approval: not found")
)

// Unauthenticated is recorded as the approver when the daemon has no operator
// identity for the request (authentication off, no single-tenant operator
// configured). Such deployments approved with an empty approver before this
// package existed; refusing them would break approvals there, and an empty
// string would hide that the approval was unattributed. The label says so.
const Unauthenticated = "unauthenticated"

// SameOrigin is THE same-origin ladder for mutating browser requests:
// Sec-Fetch-Site when the browser sends it, otherwise an Origin whose host
// equals the request host, otherwise refuse. The daemon's CSRF middleware
// (internal/api isCSRFSafe) and every approval handler call this; there is no
// second copy.
func SameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "same-site", "none":
		return true
	case "cross-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Sec-Fetch-Site and no Origin on a mutating request: no
		// same-origin signal to trust. Fail closed.
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		// A malformed Origin from a browser is a tampering signal.
		return false
	}
	return parsed.Host == r.Host
}

// CheckRequest is the first half of the gate, run before any row is read:
// POST only, and a same-origin signal.
func CheckRequest(r *http.Request) error {
	if r.Method != http.MethodPost {
		return ErrMethod
	}
	if !SameOrigin(r) {
		return ErrCrossSite
	}
	return nil
}

// Authorize is the second half, run once the row's project is known: the
// caller must be allowed that project, and the approval is attributed to the
// operator, or to Unauthenticated when the daemon has no identity for them. allowsProject and operatorID are the host server's own resolvers, so
// this package imposes no identity model of its own.
func Authorize(r *http.Request, projectID string,
	allowsProject func(*http.Request, string) bool,
	operatorID func(*http.Request) string,
) (string, error) {
	if projectID != "" && !allowsProject(r, projectID) {
		return "", ErrScope
	}
	approver := operatorID(r)
	if approver == "" {
		approver = Unauthenticated
	}
	return approver, nil
}

// WriteError answers a refused approval with its status code.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrMethod):
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case errors.Is(err, ErrCrossSite):
		http.Error(w, "CSRF: cross-site or unverifiable origin", http.StatusForbidden)
	case errors.Is(err, ErrScope):
		http.NotFound(w, r)
	default:
		http.Error(w, "approval refused", http.StatusBadRequest)
	}
}

// Canonical re-encodes JSON with sorted object keys, no insignificant
// whitespace and string escapes resolved (a \u00e9 escape and the literal
// rune encode the same). It does NOT normalise numbers: 100, 1e2 and 100.0
// keep their literal forms and hash differently, which is right for binding
// an approval to the bytes shown. Array order is significant. A document
// with a duplicate object key is refused: collapsing it would let a hash
// stand for a key the approver's view may have shown differently.
func Canonical(raw []byte) ([]byte, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("approval: canonical: %w", err)
	}
	if dec.More() {
		return nil, errors.New("approval: canonical: trailing data after the JSON value")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// encoding/json sorts map keys, which is what makes this canonical.
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("approval: canonical: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CanonicalSHA256 is THE content hash for approvals: sha256 over Canonical,
// hex-encoded. It computes the hash stored with a proposal, the hash carried
// by the approve form, and the hash re-checked before execution.
func CanonicalSHA256(raw []byte) (string, error) {
	c, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// rejectDuplicateKeys walks the token stream and refuses any object that
// repeats a key.
func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("approval: canonical: %w", err)
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return fmt.Errorf("approval: canonical: %w", err)
				}
				key, _ := keyTok.(string)
				if seen[key] {
					return fmt.Errorf("approval: canonical: duplicate object key %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // the closing delimiter
		if err != nil {
			return fmt.Errorf("approval: canonical: %w", err)
		}
		return nil
	}
	return walk()
}
