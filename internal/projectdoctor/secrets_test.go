package projectdoctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type recordingWriter struct {
	name, value string
	who         persistence.AdminAuditEntry
	err         error
}

func (w *recordingWriter) Put(_ context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	w.name, w.value, w.who = name, string(value), who
	return w.err
}

type mapLookup map[string]string

func (m mapLookup) Get(n string) (string, bool) { v, ok := m[n]; return v, ok }

// GitHub #625 item 3 / #626 E: Set writes the store, writes no file and sets
// no environment variable.
func TestStoreSecrets_SetWritesTheStoreOnly_626(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const name = "PROJECTDOCTOR_626_SECRET"
	w := &recordingWriter{}
	s := StoreSecrets{Writer: w, Who: persistence.AdminAuditEntry{Principal: "op"}}
	if err := s.Set(name, "s3cr3t"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if w.name != name || w.value != "s3cr3t" {
		t.Fatalf("stored %q=%q", w.name, w.value)
	}
	if w.who.Source != "project-setup" || w.who.Principal != "op" {
		t.Fatalf("audit identity %+v", w.who)
	}
	if os.Getenv(name) != "" {
		t.Fatal("Set must not touch the process environment")
	}
	if _, err := os.Stat(filepath.Join(dir, "project-secrets.env")); err == nil {
		t.Fatal("Set must not write an env file")
	}
}

func TestStoreSecrets_RefusesWithoutAStore_626(t *testing.T) {
	if err := (StoreSecrets{}).Set("X_626", "v"); err == nil {
		t.Fatal("no store must refuse, never fall back to a file")
	}
	w := &recordingWriter{err: errors.New("store down")}
	if err := (StoreSecrets{Writer: w}).Set("X_626", "v"); err == nil {
		t.Fatal("a failed store write is returned")
	}
}

func TestStoreSecrets_HasSeesTheStore_626(t *testing.T) {
	s := StoreSecrets{Lookup: mapLookup{"STORED_626": "v", "EMPTY_626": ""}}
	if !s.Has("STORED_626") {
		t.Fatal("a stored name is present")
	}
	if s.Has("EMPTY_626") || s.Has("ABSENT_626") || s.Has("") {
		t.Fatal("empty, absent and blank names are absent")
	}
	if (StoreSecrets{}).Has("STORED_626") {
		t.Fatal("no lookup: absent")
	}
}

// Review 1dd3 F2: an audit row always names who wrote.
func TestStoreSecrets_DefaultsThePrincipal_626(t *testing.T) {
	w := &recordingWriter{}
	if err := (StoreSecrets{Writer: w}).Set("X_626", "v"); err != nil {
		t.Fatal(err)
	}
	if w.who.Principal != "project-setup" || w.who.Source != "project-setup" {
		t.Fatalf("audit identity %+v", w.who)
	}
}
