package github

// GitHub #800 (github-app-channel design, "Install-to-onboard"). Found at:
// n/a (feature). Tests at: integration (the real webhook handler with
// signed deliveries); the bindings' rules are the persistence suite's.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func registeredChannel800(t *testing.T, hook func(context.Context, InstallationEvent) error) (*Channel, *[]string) {
	t.Helper()
	var drops []string
	ch, err := New(Config{WebhookSecret: "shhh", Registered: true, OnInstallationEvent: hook,
		OnDrop: func(r string) { drops = append(drops, r) }})
	if err != nil {
		t.Fatalf("a registered channel with no installation was refused: %v", err)
	}
	return ch, &drops
}

func TestRegistered800_InstallationEventsReachTheHook(t *testing.T) {
	var got []InstallationEvent
	ch, _ := registeredChannel800(t, func(_ context.Context, ev InstallationEvent) error { got = append(got, ev); return nil })
	created := []byte(`{"action":"created","installation":{"id":77},"repositories":[{"full_name":"Acme/Widgets"},{"full_name":"bad name/x"}]}`)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "installation", "d-1", created))
	if w.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("code %d, events %v", w.Code, got)
	}
	if ev := got[0]; ev.DeliveryID != "d-1" || ev.InstallationID != 77 || ev.Action != "created" || len(ev.Added) != 1 || ev.Added[0] != "Acme/Widgets" {
		t.Fatalf("event = %+v, want the one valid repository", ev)
	}
	removed := []byte(`{"action":"removed","installation":{"id":77},"repositories_removed":[{"full_name":"acme/widgets"}]}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "installation_repositories", "d-2", removed))
	if len(got) != 2 || got[1].Action != "removed" || len(got[1].Removed) != 1 {
		t.Fatalf("removed = %+v", got)
	}
	// An unsigned event never reaches the hook.
	bad := signedRequest("wrong", "installation", "d-3", created)
	w = httptest.NewRecorder()
	ch.HandleWebhook(w, bad)
	if w.Code != http.StatusUnauthorized || len(got) != 2 {
		t.Fatalf("an unsigned event answered %d and reached the hook", w.Code)
	}
}

func TestRegistered800_AFailedStoreAnswers500(t *testing.T) {
	ch, _ := registeredChannel800(t, func(context.Context, InstallationEvent) error { return errors.New("db down") })
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "installation", "d-1", []byte(`{"action":"created","installation":{"id":77},"repositories":[]}`)))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed store answered %d, want 500", w.Code)
	}
}

func TestRegistered800_BoundRepositoriesRouteCaseInsensitively(t *testing.T) {
	ch, drops := registeredChannel800(t, nil)
	tc := &stubTaskCreator{}
	ch.SetRegisteredRoutes([]RegisteredRoute{{Repo: "acme/widgets", InstallationConfig: InstallationConfig{
		ProjectID: "gh-acme-widgets", InstallationID: 77, RepoAllowlist: []string{"acme/widgets"}, TaskCreator: tc}}})
	pr := func(installation string) []byte {
		return []byte(`{"action":"opened","repository":{"full_name":"Acme/Widgets"},"sender":{"login":"x"},
			"installation":{"id":` + installation + `},"pull_request":{"number":3,"title":"t","body":"b"}}`)
	}
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "pull_request", "d-pr", pr("77")))
	if w.Code != http.StatusOK || len(tc.copyEvents()) != 1 {
		t.Fatalf("a bound repository in GitHub's casing was not reviewed: code %d, events %d", w.Code, len(tc.copyEvents()))
	}
	// The same repository from another installation is dropped.
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "pull_request", "d-pr2", pr("78")))
	if len(tc.copyEvents()) != 1 || len(*drops) != 1 || (*drops)[0] != "other_installation" {
		t.Fatalf("another installation's delivery: events %d drops %v", len(tc.copyEvents()), *drops)
	}
	// A mention is not answered for an App-backed project.
	comment := []byte(`{"action":"created","repository":{"full_name":"acme/widgets"},"sender":{"login":"x"},"installation":{"id":77},
		"issue":{"number":3,"title":"t"},"comment":{"body":"@vornik hi","user":{"login":"x"}}}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-c", comment))
	if len(tc.copyEvents()) != 1 {
		t.Fatal("a mention on an App-backed project started work")
	}
	// An unbound repository with an unknown installation is not routed to
	// anything, even with no other route (no sole-installation fallback).
	other := []byte(`{"action":"opened","repository":{"full_name":"acme/other"},"sender":{"login":"x"},"installation":{"id":99},"pull_request":{"number":1}}`)
	w = httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "pull_request", "d-o", other))
	if w.Code != http.StatusOK || len(tc.copyEvents()) != 1 {
		t.Fatalf("an unbound repository was routed: code %d", w.Code)
	}
}

// A registered channel beside a hand-made single-project route never sends
// an unknown installation id to that route (the as-built fallback).
func TestRegistered800_NoSoleInstallationFallback(t *testing.T) {
	tc := &stubTaskCreator{}
	ch, err := New(Config{WebhookSecret: "shhh", Registered: true, RepoAllowlist: []string{"acme/api"}, InstallationID: 9001, TaskCreator: tc})
	if err != nil {
		t.Fatal(err)
	}
	pr := []byte(`{"action":"opened","repository":{"full_name":"acme/api"},"sender":{"login":"x"},"installation":{"id":4242},"pull_request":{"number":1}}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "pull_request", "d-x", pr))
	if n := len(tc.copyEvents()); n != 0 {
		t.Fatalf("an unknown installation id reached the hand-made route (%d events)", n)
	}
}

// review-20261118-a33a F2: a bound repository that is not routable is
// counted by why and never handed to a hand-made route.
func TestRegistered800_ABoundButPausedRepositoryIsCountedNotHandedOn(t *testing.T) {
	tc := &stubTaskCreator{}
	var drops []string
	ch, err := New(Config{WebhookSecret: "shhh", Registered: true, RepoAllowlist: []string{"acme/widgets"}, InstallationID: 77,
		TaskCreator: tc, OnDrop: func(r string) { drops = append(drops, r) }})
	if err != nil {
		t.Fatal(err)
	}
	ch.SetRegisteredRoutes([]RegisteredRoute{{Repo: "acme/widgets", DropReason: "paused", InstallationConfig: InstallationConfig{InstallationID: 77}}})
	pr := []byte(`{"action":"opened","repository":{"full_name":"acme/widgets"},"sender":{"login":"x"},"installation":{"id":77},"pull_request":{"number":1}}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "pull_request", "d", pr))
	if len(tc.copyEvents()) != 0 || len(drops) != 1 || drops[0] != "paused" {
		t.Fatalf("a paused bound repository: events %d, drops %v", len(tc.copyEvents()), drops)
	}
}
