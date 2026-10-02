package secretstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type envMap map[string]string

func (e envMap) Get(n string) (string, bool) { v, ok := e[n]; return v, ok && v != "" }

func TestSource_RoutesByForm(t *testing.T) {
	st := newStore(t, newMemRepo(), bytes.Repeat([]byte{1}, 32))
	_ = st.Put(context.Background(), "hermes", "FIO", "secret", []byte("store-value"), "d")
	src := Source{Store: st, Env: envMap{"GITHUB_TOKEN": "env-value", "hermes/FIO": "WRONG-env"}}
	if v, ok := src.Get("hermes/FIO"); !ok || v != "store-value" {
		t.Fatalf("namespaced = (%q, %v)", v, ok)
	}
	if v, ok := src.Get("GITHUB_TOKEN"); !ok || v != "env-value" {
		t.Fatalf("flat = (%q, %v)", v, ok)
	}
	if _, ok := src.Get("hermes/MISSING"); ok {
		t.Fatal("missing namespaced secret resolved")
	}
}

// Review Focus 2: a key failure is reported through OnError, not swallowed
// as "missing", so the wiring layer can log the real cause.
func TestSource_ReportsKeyFailure(t *testing.T) {
	repo := newMemRepo()
	_ = newStore(t, repo, bytes.Repeat([]byte{1}, 32)).Put(context.Background(), "hermes", "FIO", "secret", []byte("v"), "d")
	var got error
	src := Source{Store: newStore(t, repo, bytes.Repeat([]byte{2}, 32)), Env: envMap{}, OnError: func(_ string, err error) { got = err }}
	if _, ok := src.Get("hermes/FIO"); ok {
		t.Fatal("opened under the wrong key")
	}
	if !errors.Is(got, ErrKeyUnavailable) {
		t.Fatalf("OnError got %v, want ErrKeyUnavailable", got)
	}
}

func TestSource_NilStoreRefusesNamespaced(t *testing.T) {
	src := Source{Env: envMap{"hermes/FIO": "env"}}
	if _, ok := src.Get("hermes/FIO"); ok {
		t.Fatal("a namespaced name resolved from the environment")
	}
}

// Plan review f329 finding 8 (Review Focus 2): rows exist but the key failed
// to load at boot. A namespaced lookup reports the key error, never a plain miss.
func TestSource_KeyErrIsReported(t *testing.T) {
	var got error
	src := Source{KeyErr: ErrKeyUnavailable, Env: envMap{}, OnError: func(_ string, err error) { got = err }}
	if _, ok := src.Get("hermes/FIO"); ok {
		t.Fatal("resolved despite a key error")
	}
	if !errors.Is(got, ErrKeyUnavailable) {
		t.Fatalf("OnError got %v, want ErrKeyUnavailable", got)
	}
	if v, ok := src.Get("FLAT"); ok || v != "" {
		t.Fatal("flat lookup misbehaved")
	}
}
