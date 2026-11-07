// Package webpush sends Web Push notifications to approver devices
// (agent-administered design part 09 §9.3a, GitHub #163). The endpoint check
// limits the daemon to the browsers' own push services. Messages are
// encrypted per RFC 8291 and signed with VAPID per RFC 8292.
package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// randReader is the entropy source for keys, salts and signatures.
var randReader = rand.Reader

// recordSize is the aes128gcm record size the header states (RFC 8188). A
// push message is one record, well under it.
const recordSize = 4096

// MaxEndpointLen bounds a subscription's endpoint (design §9.3a B.4).
const MaxEndpointLen = 1024

// Encrypt encrypts plaintext for a subscription (RFC 8291) with a fresh
// sender key and salt, and returns the aes128gcm body: the 86-byte header
// followed by the one record.
func Encrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(randReader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(randReader, salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, uaPublic, authSecret, asPriv, salt)
}

func encrypt(plaintext, uaPublic, authSecret []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(authSecret) != 16 {
		return nil, errors.New("webpush: auth secret must be 16 bytes")
	}
	if len(plaintext) > recordSize-16-1-86 {
		return nil, errors.New("webpush: message too long")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: subscription key: %w", err)
	}
	secret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPublic := asPriv.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 86+len(plaintext)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPublic)))
	out = append(out, asPublic...)
	// The last (and only) record ends with the 0x02 delimiter.
	return gcm.Seal(out, nonce, append(append([]byte{}, plaintext...), 2), nil), nil
}

// KeyFromSeed turns a 32-byte seed (derived from the secret store's master
// key) into the VAPID P-256 key, deterministically. A seed that is not a
// valid scalar is hashed again, which a real seed needs with probability
// about 2^-32.
func KeyFromSeed(seed []byte) (*ecdsa.PrivateKey, error) {
	if len(seed) != 32 {
		return nil, errors.New("webpush: VAPID seed must be 32 bytes")
	}
	d := seed
	for range 8 {
		if k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d); err == nil {
			return k, nil
		}
		h := sha256.Sum256(d)
		d = h[:]
	}
	return nil, errors.New("webpush: no valid VAPID key from seed")
}

// VAPID signs the RFC 8292 header with the daemon's key.
type VAPID struct {
	key *ecdsa.PrivateKey
	sub string
	pub string
}

// NewVAPID returns a signer whose tokens name sub (the public origin, or a
// mailto: address) as the contact.
func NewVAPID(key *ecdsa.PrivateKey, sub string) *VAPID {
	pub, _ := key.PublicKey.Bytes()
	return &VAPID{key: key, sub: sub, pub: base64.RawURLEncoding.EncodeToString(pub)}
}

// PublicKey is the application server key a browser subscribes with,
// base64url without padding.
func (v *VAPID) PublicKey() string { return v.pub }

// Header returns the Authorization value for a push to endpoint: an ES256
// token with aud the endpoint's origin and exp 12 hours after now.
func (v *VAPID) Header(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("webpush: bad endpoint")
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": v.sub,
	})
	if err != nil {
		return "", err
	}
	signing := head + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(randReader, v.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + v.pub, nil
}

// pushHosts are the push services accepted exactly (design §9.3a B.4);
// windowsSuffix is matched with at least one label before it.
var pushHosts = map[string]bool{
	"fcm.googleapis.com":                true,
	"updates.push.services.mozilla.com": true,
	"web.push.apple.com":                true,
}

const windowsSuffix = ".notify.windows.com"

// ErrEndpoint is returned for an endpoint the daemon will not post to.
var ErrEndpoint = errors.New("webpush: endpoint is not a supported push service")

// ValidateEndpoint accepts only an https URL on the default port, without
// user info, whose host is one of the browsers' push services. It is what
// stops a device from making the daemon post to an address of its choosing.
func ValidateEndpoint(raw string) error {
	if raw == "" || len(raw) > MaxEndpointLen {
		return ErrEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return ErrEndpoint
	}
	host := strings.ToLower(u.Hostname())
	if pushHosts[host] {
		return nil
	}
	if label, ok := strings.CutSuffix(host, windowsSuffix); ok && label != "" &&
		!strings.HasPrefix(label, ".") && !strings.HasSuffix(label, ".") && !strings.Contains(label, "..") {
		return nil
	}
	return ErrEndpoint
}

// ValidateKeys checks a subscription's keys: p256dh a P-256 point, auth 16
// bytes, both base64url with or without padding.
func ValidateKeys(p256dh, auth string) error {
	p, err := DecodeKey(p256dh)
	if err != nil {
		return errors.New("webpush: p256dh is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(p); err != nil {
		return errors.New("webpush: p256dh is not a P-256 point")
	}
	a, err := DecodeKey(auth)
	if err != nil || len(a) != 16 {
		return errors.New("webpush: auth must be 16 bytes")
	}
	return nil
}

// DecodeKey decodes base64url with or without padding.
func DecodeKey(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
