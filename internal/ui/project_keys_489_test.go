package ui

// GitHub #489 (18-security-and-tenancy.md, amendment 2026-11-15): a retired
// key (revoked or expired) can be removed from the keys page at once, with
// an audit row; a live key cannot. In the All view an expired key read
// "active" and offered Revoke. Found at: production (operator, 2026-10-21:
// dead rows the operator could not remove). Regression test at: integration
// (the console handler with the real gate).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func (r keysRig754) seedRetired(t *testing.T, id string, revoked bool) {
	t.Helper()
	k := &persistence.APIKey{ID: id, ProjectID: "assistant", Name: "name-" + id, KeyHash: "h-" + id,
		KeyPrefix: "sk-vornik-" + id[:2], CreatedAt: time.Now().UTC().Add(-time.Hour), CreatedBy: "cli"}
	past := time.Now().UTC().Add(-time.Minute)
	if revoked {
		k.RevokedAt = &past
	} else {
		k.ExpiresAt = &past
	}
	if err := r.mem.Create(context.Background(), k); err != nil {
		t.Fatal(err)
	}
}

func TestProjectKeys489_AllViewLabelsRetiredKeysAndOffersRemove(t *testing.T) {
	r := newKeysRig()
	r.seed(t, "akey-live", "assistant")
	r.seedRetired(t, "akey-gone", true)
	r.seedRetired(t, "akey-old", false)
	body := r.do(t, http.MethodGet, keysPath754+"?status=all", owner754, nil).Body.String()
	row := func(id string) string {
		i := strings.Index(body, "name-"+id)
		if i < 0 {
			if i = strings.Index(body, ">"+id+"<"); i < 0 {
				t.Fatalf("the All view lacks %s", id)
			}
		}
		end := strings.Index(body[i:], "</tr>")
		return body[i : i+end]
	}
	if old := row("akey-old"); !strings.Contains(old, "expired") || strings.Contains(old, `value="revoke"`) || !strings.Contains(old, `value="remove"`) {
		t.Errorf("an expired key should read expired and offer Remove, not Revoke: %s", old)
	}
	if gone := row("akey-gone"); !strings.Contains(gone, `value="remove"`) {
		t.Errorf("a revoked key should offer Remove: %s", gone)
	}
	if live := row("akey-live"); strings.Contains(live, `value="remove"`) || !strings.Contains(live, `value="revoke"`) {
		t.Errorf("a live key should offer Revoke, not Remove: %s", live)
	}
}

func TestProjectKeys489_OwnerRemovesARetiredKey(t *testing.T) {
	r := newKeysRig()
	r.seedRetired(t, "akey-gone", true)
	rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"remove"}, "key_id": {"akey-gone"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Removed key") {
		t.Fatalf("remove: %d, said Removed: %v", rec.Code, strings.Contains(rec.Body.String(), "Removed key"))
	}
	if _, err := r.mem.GetByID(context.Background(), "akey-gone"); err == nil {
		t.Fatal("the removed key is still stored")
	}
	rows := r.rows("key.remove")
	if len(rows) != 1 || rows[0].Target != "akey-gone" || rows[0].Principal != "console:u-owner" {
		t.Fatalf("want one key.remove row naming the key and the owner, got %+v", r.audit.rows)
	}
	raw, _ := json.Marshal(rows[0])
	for _, want := range []string{"assistant", "name-akey-gone", "revoked_at"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the key.remove row lacks %q: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "sk-vornik") || strings.Contains(string(raw), "h-akey-gone") {
		t.Error("the key.remove row carries the key's prefix or hash")
	}
}

func TestProjectKeys489_ALiveKeyIsNotRemoved(t *testing.T) {
	r := newKeysRig()
	r.seed(t, "akey-live", "assistant")
	r.seedRetired(t, "akey-else", true)
	rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"remove"}, "key_id": {"akey-live"}})
	if !strings.Contains(rec.Body.String(), "This key is still live. Revoke it first.") {
		t.Errorf("removing a live key should say it is still live (status %d)", rec.Code)
	}
	if _, err := r.mem.GetByID(context.Background(), "akey-live"); err != nil {
		t.Fatal("a live key was removed")
	}
	if len(r.rows("key.remove")) != 0 {
		t.Error("a refused remove wrote a key.remove row")
	}
	// The IDOR guard: another project's retired key is not this page's.
	_ = r.mem.Create(context.Background(), &persistence.APIKey{ID: "akey-other", ProjectID: "other", Name: "o", KeyHash: "h-o", KeyPrefix: "sk-vornik-o", CreatedAt: time.Now().UTC(), RevokedAt: ptrTime(time.Now().UTC())})
	r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"remove"}, "key_id": {"akey-other"}})
	if _, err := r.mem.GetByID(context.Background(), "akey-other"); err != nil {
		t.Fatal("the keys page of one project removed another project's key")
	}
}

func TestProjectKeys489_AMemberCannotRemove(t *testing.T) {
	r := newKeysRig()
	r.seedRetired(t, "akey-gone", true)
	if rec := r.do(t, http.MethodPost, keysPath754, member754, url.Values{"action": {"remove"}, "key_id": {"akey-gone"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a project member's remove: %d, want 403", rec.Code)
	}
	if _, err := r.mem.GetByID(context.Background(), "akey-gone"); err != nil {
		t.Fatal("a project member removed a key")
	}
}

// Code review 149d F1: with no audit sink the page removes nothing.
func TestProjectKeys489_RemoveRefusedWithoutAudit(t *testing.T) {
	mem := &uiMemAPIKeyRepo{}
	r := keysRig754{mem: mem, repo: mem, audit: &recordingAudit623{}}
	r.srv = NewServer(WithAPIKeyRepository(mem))
	r.seedRetired(t, "akey-gone", true)
	body := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"remove"}, "key_id": {"akey-gone"}}).Body.String()
	if !strings.Contains(body, "needs the admin audit") {
		t.Error("the page should say why it did not remove the key")
	}
	if _, err := mem.GetByID(context.Background(), "akey-gone"); err != nil {
		t.Fatal("a key was removed with no audit record")
	}
}
