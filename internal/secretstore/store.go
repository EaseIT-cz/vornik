package secretstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
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
	row, err := s.PreparePut(ns, name, kind, value, device)
	if err != nil {
		return err
	}
	return s.repo.Upsert(ctx, *row)
}

// PreparePut seals an agent-owned row without publishing it.
func (s *Store) PreparePut(ns, name, kind string, value []byte, device string) (*persistence.AgentSecretRow, error) {
	if err := check(ns, name); err != nil {
		return nil, err
	}
	if kind != "secret" && kind != "oauth_token" {
		return nil, fmt.Errorf("secretstore: unknown kind %q", kind)
	}
	return s.seal(ns, name, kind, value, device)
}

// seal builds a sealed row for (ns, name). Callers check the namespace and
// name first: the agent paths with check, the operator paths with their own
// reserved namespace.
func (s *Store) seal(ns, name, kind string, value []byte, device string) (*persistence.AgentSecretRow, error) {
	if len(value) == 0 {
		// Source reads an empty value as unset; refuse it here so no row
		// exists that reads as missing.
		return nil, fmt.Errorf("secretstore: empty value for %s/%s", ns, name)
	}
	a, err := s.aead(ns)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	now := s.now()
	return &persistence.AgentSecretRow{
		Namespace: ns, Name: name, Kind: kind,
		Ciphertext: a.Seal(nil, nonce, value, []byte(ns+"/"+name)), Nonce: nonce,
		CreatedByDevice: device, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Get opens the credential at (ns, name).
func (s *Store) Get(ctx context.Context, ns, name string) ([]byte, error) {
	if err := check(ns, name); err != nil {
		return nil, err
	}
	return s.open(ctx, ns, name)
}

// open reads and opens (ns, name). Callers check the namespace and name.
func (s *Store) open(ctx context.Context, ns, name string) ([]byte, error) {
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
	return s.list(ctx, ns)
}

func (s *Store) list(ctx context.Context, ns string) ([]Meta, error) {
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

// aimarkSigningSalt and aimarkSigningInfo are the HKDF domain of the AI
// output-marking key (GitHub #117; Art 50 design §14.7). Its own salt keeps
// it apart from every encryption key by construction (review 3701 F1). The
// "k1" in the info is the key epoch: a rotation derives "k2" beside it.
const (
	aimarkSigningSalt = "vornik-aimark-signing"
	aimarkSigningInfo = "aimark/ed25519/k1"
)

// AIMarkSigningSeed derives the Ed25519 seed that signs AI output marks.
// Every daemon sharing the master key derives the same seed.
func (s *Store) AIMarkSigningSeed() ([]byte, error) {
	return hkdf.Key(sha256.New, s.master, []byte(aimarkSigningSalt), aimarkSigningInfo, ed25519.SeedSize)
}

// operatorCredentialsNamespace holds the credentials an operator (or a managed
// instance's tenant_admin) enters in the console, which an un-namespaced
// secret://NAME reference reads after the environment (GitHub #623). It fails
// agentns.Valid, so check refuses it on every agent path and no
// secret://<ns>/NAME reference can name it; its HKDF domain is its own, apart
// from the _operator sealing domain.
// see LLD § https://docs.vornik.io, Amendment 2026-11-02 (#623)
const operatorCredentialsNamespace = "_operator_credentials"

func checkOperatorName(name string) error {
	if !agentns.ValidSecretName(name) {
		return fmt.Errorf("secretstore: invalid credential name %q (use A-Z, 0-9 and _, starting with a letter)", name)
	}
	return nil
}

// PutOperatorCredential seals value under name; actor is the console
// principal, recorded in created_by_device.
func (s *Store) PutOperatorCredential(ctx context.Context, name string, value []byte, actor string) error {
	if err := checkOperatorName(name); err != nil {
		return err
	}
	row, err := s.seal(operatorCredentialsNamespace, name, "secret", value, actor)
	if err != nil {
		return err
	}
	return s.repo.Upsert(ctx, *row)
}

// GetOperatorCredential opens the operator credential name.
func (s *Store) GetOperatorCredential(ctx context.Context, name string) ([]byte, error) {
	if err := checkOperatorName(name); err != nil {
		return nil, err
	}
	return s.open(ctx, operatorCredentialsNamespace, name)
}

// ListOperatorCredentials returns the operator credentials' metadata, never a
// value.
func (s *Store) ListOperatorCredentials(ctx context.Context) ([]Meta, error) {
	return s.list(ctx, operatorCredentialsNamespace)
}

// DeleteOperatorCredential removes name; a missing one is not an error.
func (s *Store) DeleteOperatorCredential(ctx context.Context, name string) error {
	if err := checkOperatorName(name); err != nil {
		return err
	}
	return s.repo.Delete(ctx, operatorCredentialsNamespace, name)
}
