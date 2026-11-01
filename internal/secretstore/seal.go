package secretstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// Sealing values stored outside the secret table (plan P4.4): an agent
// project's OAuth tokens live in their own row, sealed with the namespace's
// key. The additional data is "<ns>/<label>", so a sealed value cannot be
// moved to another namespace, server or column. A label always contains a
// "/", which no secret name can, so it never collides with a stored
// secret's additional data.

// SealedPrefix marks a sealed value.
const SealedPrefix = "sv1:"

// ErrNotSealed means a value that must be sealed is not.
var ErrNotSealed = errors.New("secretstore: the value is not sealed")

// Seal seals plain for namespace ns under label. An empty value stays empty.
func (s *Store) Seal(ns, label string, plain []byte) (string, error) {
	if len(plain) == 0 {
		return "", nil
	}
	if err := checkLabel(ns, label); err != nil {
		return "", err
	}
	a, err := s.aead(ns)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := a.Seal(nonce, nonce, plain, []byte(ns+"/"+label))
	return SealedPrefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// Open opens a value Seal produced for the same ns and label.
func (s *Store) Open(ns, label, sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, nil
	}
	raw, ok := strings.CutPrefix(sealed, SealedPrefix)
	if !ok {
		return nil, ErrNotSealed
	}
	if err := checkLabel(ns, label); err != nil {
		return nil, err
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("secretstore: malformed sealed value")
	}
	a, err := s.aead(ns)
	if err != nil {
		return nil, err
	}
	if len(b) < a.NonceSize() {
		return nil, fmt.Errorf("secretstore: malformed sealed value")
	}
	plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte(ns+"/"+label))
	if err != nil {
		return nil, fmt.Errorf("secretstore: the sealed value does not open for %s/%s", ns, label)
	}
	return plain, nil
}

func checkLabel(ns, label string) error {
	if !agentns.Valid(ns) {
		return fmt.Errorf("secretstore: invalid namespace %q", ns)
	}
	if !strings.Contains(label, "/") || strings.ContainsAny(label, "\x00\n") {
		return fmt.Errorf("secretstore: invalid label %q", label)
	}
	return nil
}

// operatorDomain is the key domain of values that belong to no agent
// namespace: operator projects' and the daemon scope's OAuth tokens (GitHub
// #571; mcp-server-authentication-design.md §15.2 A1). It fails
// agentns.Valid by construction, so no agent namespace derives this key.
const operatorDomain = "_operator"

// SealOperator seals plain in the operator domain under label. An empty value
// stays empty. It does not go through Seal, whose namespace check refuses the
// domain; only the label checks apply.
func (s *Store) SealOperator(label string, plain []byte) (string, error) {
	if len(plain) == 0 {
		return "", nil
	}
	if err := checkOperatorLabel(label); err != nil {
		return "", err
	}
	a, err := s.aead(operatorDomain)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := a.Seal(nonce, nonce, plain, []byte(operatorDomain+"/"+label))
	return SealedPrefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// OpenOperator opens a value SealOperator produced for the same label.
func (s *Store) OpenOperator(label, sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, nil
	}
	raw, ok := strings.CutPrefix(sealed, SealedPrefix)
	if !ok {
		return nil, ErrNotSealed
	}
	if err := checkOperatorLabel(label); err != nil {
		return nil, err
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("secretstore: malformed sealed value")
	}
	a, err := s.aead(operatorDomain)
	if err != nil {
		return nil, err
	}
	if len(b) < a.NonceSize() {
		return nil, fmt.Errorf("secretstore: malformed sealed value")
	}
	plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte(operatorDomain+"/"+label))
	if err != nil {
		return nil, fmt.Errorf("secretstore: the sealed value does not open for the operator domain")
	}
	return plain, nil
}

func checkOperatorLabel(label string) error {
	if !strings.Contains(label, "/") || strings.ContainsAny(label, "\x00\n") {
		return fmt.Errorf("secretstore: invalid label %q", label)
	}
	return nil
}

// ErrNothingSampled means credentials were counted but none could be listed,
// so no value was opened and the key is unproven either way.
var ErrNothingSampled = errors.New("secretstore: rows were counted but none could be listed to sample")

// SampleOpens opens one stored agent secret: one sampled row tells a wrong key
// from a right one, since every row is sealed under keys derived from the same
// master. A wrong key is ErrKeyUnavailable. Shared by the secret_store_key
// doctor check and the token seal pass (GitHub #571 A3).
func (s *Store) SampleOpens(ctx context.Context) error {
	nss, err := s.repo.ListNamespaces(ctx)
	if err != nil {
		return err
	}
	if len(nss) == 0 {
		return ErrNothingSampled
	}
	metas, err := s.List(ctx, nss[0])
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		return ErrNothingSampled
	}
	_, err = s.Get(ctx, nss[0], metas[0].Name)
	return err
}
