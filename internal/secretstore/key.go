// Package secretstore seals agent credentials (agent-administered Vornik
// design §8.1): AES-256-GCM under a per-namespace key derived with HKDF from
// one master key on disk. The master key never enters the database, a log
// line, an error or an API response.
//
// Recovery: a crash between creating store.key.tmp and linking it leaves a
// stale .tmp; LoadOrCreateKey removes one older than staleTmpAge before its
// own exclusive create, so first use is never blocked by it.
package secretstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	keySize     = 32
	staleTmpAge = 2 * time.Minute
)

// ErrKeyUnavailable means the master key is absent, unreadable, the wrong
// size, or not the key a row was sealed under. It is never reported as a
// missing secret.
var ErrKeyUnavailable = errors.New("secret store key unavailable")

// LoadKey reads an existing master key.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrKeyUnavailable, path, err)
	}
	if len(b) != keySize {
		return nil, fmt.Errorf("%w: %s holds %d bytes, want %d", ErrKeyUnavailable, path, len(b), keySize)
	}
	return b, nil
}

// LoadOrCreateKey loads the master key, creating it on first use. The new key
// is written to a .tmp file opened O_EXCL and then hard-linked into place;
// a link fails when the target exists, so of concurrent first users exactly
// one key wins and every caller reads that winner's bytes.
func LoadOrCreateKey(path string) ([]byte, error) {
	if k, err := LoadKey(path); err == nil {
		return k, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
	}
	tmp := path + ".tmp"
	if st, err := os.Stat(tmp); err == nil && time.Since(st.ModTime()) > staleTmpAge {
		_ = os.Remove(tmp)
	}
	k := make([]byte, keySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_, werr := f.Write(k)
		cerr := f.Close()
		if werr == nil && cerr == nil {
			if lerr := os.Link(tmp, path); lerr != nil && !os.IsExist(lerr) {
				_ = os.Remove(tmp)
				return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, lerr)
			}
		}
		_ = os.Remove(tmp)
	}
	return loadWithRetry(path)
}

// loadWithRetry covers the window where another caller holds the .tmp file
// and has not linked it yet.
func loadWithRetry(path string) ([]byte, error) {
	var last error
	for i := 0; i < 400; i++ {
		k, err := LoadKey(path)
		if err == nil {
			return k, nil
		}
		last = err
		time.Sleep(5 * time.Millisecond)
	}
	return nil, last
}
