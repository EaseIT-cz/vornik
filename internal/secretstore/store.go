package secretstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

// ErrNotFound means no credential is stored under (namespace, name).
var ErrNotFound = errors.New("agent secret not set")

const hkdfSalt = "vornik-secretstore-v1"

// Meta is what may be shown about a credential: never its value.
type Meta struct {
	Name, Kind, CreatedByDevice string
	UpdatedAt                   time.Time
}

// Store seals and opens agent credentials.
type Store struct {
	repo   persistence.AgentSecretRepository
	master []byte
	now    func() time.Time
}

// New returns a Store over repo with the given master key.
func New(repo persistence.AgentSecretRepository, master []byte) (*Store, error) {
	if len(master) != keySize {
		return nil, fmt.Errorf("%w: master key is %d bytes", ErrKeyUnavailable, len(master))
	}
	return &Store{repo: repo, master: master, now: func() time.Time { return time.Now().UTC() }}, nil
}

// aead derives the namespace's key: HKDF-SHA256(master, salt, info = ns).
func (s *Store) aead(ns string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, s.master, []byte(hkdfSalt), ns, keySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func check(ns, name string) error {
	if !agentns.Valid(ns) {
		return fmt.Errorf("secretstore: invalid namespace %q", ns)
	}
	if !agentns.ValidSecretName(name) {
		return fmt.Errorf("secretstore: invalid secret name %q", name)
	}
	return nil
}

// Put seals value and stores it under (ns, name).
func (s *Store) Put(ctx context.Context, ns, name, kind string, value []byte, device string) error {
	if err := check(ns, name); err != nil {
		return err
	}
	if kind != "secret" && kind != "oauth_token" {
		return fmt.Errorf("secretstore: unknown kind %q", kind)
	}
	if len(value) == 0 {
		// Source reads an empty value as unset; refuse it here so no row
		// exists that reads as missing.
		return fmt.Errorf("secretstore: empty value for %s/%s", ns, name)
	}
	a, err := s.aead(ns)
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	now := s.now()
	return s.repo.Upsert(ctx, persistence.AgentSecretRow{
		Namespace: ns, Name: name, Kind: kind,
		Ciphertext: a.Seal(nil, nonce, value, []byte(ns+"/"+name)), Nonce: nonce,
		CreatedByDevice: device, CreatedAt: now, UpdatedAt: now,
	})
}

// Get opens the credential at (ns, name).
func (s *Store) Get(ctx context.Context, ns, name string) ([]byte, error) {
	if err := check(ns, name); err != nil {
		return nil, err
	}
	row, err := s.repo.Get(ctx, ns, name)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, ns, name)
	}
	if err != nil {
		return nil, err
	}
	a, err := s.aead(ns)
	if err != nil {
		return nil, err
	}
	// The additional data is taken from the ROW on purpose, while the key is
	// derived from the REQUESTED namespace: a row moved between namespaces
	// then fails twice over (wrong key, wrong AD). Do not "simplify" this to
	// the request's arguments; that would weaken the cross-namespace check.
	out, err := a.Open(nil, row.Nonce, row.Ciphertext, []byte(row.Namespace+"/"+row.Name))
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s does not open under this key", ErrKeyUnavailable, ns, name)
	}
	return out, nil
}

// List returns the namespace's credential metadata, ordered by name.
func (s *Store) List(ctx context.Context, ns string) ([]Meta, error) {
	if !agentns.Valid(ns) {
		return nil, fmt.Errorf("secretstore: invalid namespace %q", ns)
	}
	rows, err := s.repo.List(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(rows))
	for _, r := range rows {
		out = append(out, Meta{Name: r.Name, Kind: r.Kind, CreatedByDevice: r.CreatedByDevice, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// Delete removes (ns, name); deleting a missing credential is not an error.
func (s *Store) Delete(ctx context.Context, ns, name string) error {
	if err := check(ns, name); err != nil {
		return err
	}
	return s.repo.Delete(ctx, ns, name)
}
