package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
)

// Slice F (drift design, 2026-09-25): an acknowledgement records who decided.
// ackActor never returns "" (reserved for pre-slice-F lines) and never a
// credential.

func TestAckActor_EveryBranchIsNonEmptyAndNoCredential(t *testing.T) {
	bg := context.Background()
	session := func(sub string) context.Context {
		return context.WithValue(bg, identityKey, &auth.Identity{Subject: sub, Extra: map[string]any{auth.ExtraSessionRole: "admin"}})
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"auth off", ContextWithAuthEnabled(bg, false), "auth-disabled"},
		{"database key", context.WithValue(bg, apiKeyIDKey, "key-7"), "api_key_id:key-7"},
		{"static key", context.WithValue(bg, apiKeyKey, "sk-vornik-secret"), "api_key_sha256:"},
		{"session admin", session("gh:4242"), "session:gh:4242"},
		{"session admin without subject", session(""), "session-without-subject"},
		{"nothing identified", bg, "unidentified-admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ackActor(c.ctx)
			if got == "" || !strings.HasPrefix(got, c.want) {
				t.Fatalf("ackActor = %q, want prefix %q", got, c.want)
			}
			if strings.Contains(got, "sk-vornik-secret") {
				t.Fatal("the actor must never carry the key itself")
			}
		})
	}
	// A static-key caller whose identity Subject IS the key (as it is for the
	// static backend) and who also looks like a session admin must NOT take
	// the session branch: key presence decides first.
	both := context.WithValue(session("sk-vornik-secret"), apiKeyKey, "sk-vornik-secret")
	if got := ackActor(both); strings.HasPrefix(got, "session:") || strings.Contains(got, "sk-vornik-secret") {
		t.Fatalf("a presented key must never reach the session branch: %q", got)
	}
}

func TestAckDoctorFinding_RecordsWhoAcknowledged(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	rec := ackCall(t, h, "sk-admin", `{"check":"config_template_drift","file":"workflows/w.md"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out ackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Actor, "api_key_sha256:") {
		t.Fatalf("the response must name the actor: %s (%v)", rec.Body, err)
	}
	for _, p := range []string{".template-acks", filepath.Join(".origin", ".acks.journal")} {
		body, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || !strings.Contains(string(body), out.Actor) || strings.Contains(string(body), "sk-admin") {
			t.Fatalf("%s must name the actor and never the key: %q %v", p, body, err)
		}
	}
	// The journal-ahead-of-store sentence names who, from the journal line.
	if err := os.Remove(filepath.Join(dir, ".template-acks")); err != nil {
		t.Fatal(err)
	}
	got := h.checkConfigTemplateDrift()
	if !strings.Contains(strings.Join(got.Items, "\n"), "acknowledged by "+out.Actor+" on ") {
		t.Fatalf("the missing-ack sentence must name the actor: %v", got.Items)
	}
}

// A refused ack writes neither file: no empty-actor record can be created by
// a caller the gate turns away.
func TestAckDoctorFinding_RefusalWritesNeitherFile(t *testing.T) {
	dir := ackTree(t)
	h := ackHandler(dir)
	if rec := ackCall(t, h, "sk-someone", `{"check":"config_template_drift","file":"workflows/w.md"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
	for _, p := range []string{".template-acks", filepath.Join(".origin", ".acks.journal")} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("a refused ack wrote %s", p)
		}
	}
}
