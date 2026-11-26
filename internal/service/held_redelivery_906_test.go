package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #906: GitHub does not retry a webhook delivery, so one the held
// daemon refused with 503 was lost. The release now redelivers it (deploy
// idle gate design §13.13). Found at: review (#586 design review ad27 #4,
// observed at production: an upgrade's held minutes drop every GitHub
// event). Regression test at: integration (a real held container through
// Container.Run, released on the daemon's real mux, its pass talking to a
// fake GitHub that replays the payload into the live channel). Lower
// because: production — GitHub itself cannot be in a test.

func signed906(delivery string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte("shhh906"))
	_, _ = mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", delivery)
	return req
}

func TestHeldStart906_ReleaseRedeliversWhatTheHoldRefused(t *testing.T) {
	h := startHeld586(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"action":"labeled","repository":{"full_name":"acme/api"},"sender":{"login":"op"},
		"installation":{"id":906},"issue":{"number":7,"title":"t","body":"b","labels":[{"name":"vornik-task"}]},
		"label":{"name":"vornik-task"}}`)
	refusedAt := time.Now().UTC()

	// The daemon's own task creator: it dedupes a delivery by its
	// idempotency key github-app:<guid>, in the task table.
	if h.c.githubTaskCreatorFor == nil {
		t.Fatal("the container built no GitHub task creator")
	}
	tc := h.c.githubTaskCreatorFor(h.c.Registry.GetProject("auto894"))
	tasks := func() int {
		n := 0
		if task, err := h.c.repos.Tasks.GetByIdempotencyKey(context.Background(), "auto894", "github-app:guid-906"); err == nil && task != nil {
			n = 1
		}
		return n
	}
	var ch *github.Channel
	var mu sync.Mutex
	var posted []string
	// The fake GitHub: one delivery the held daemon refused (503), and its
	// redelivery replays the payload into the daemon's channel, as GitHub
	// would through the webhook route.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 77, "guid": "guid-906", "delivered_at": refusedAt.Format(time.RFC3339),
				"status_code": 503, "status": "Invalid HTTP Response: 503", "event": "issues"}})
		case r.Method == http.MethodPost && r.URL.Path == "/app/hook/deliveries/77/attempts":
			mu.Lock()
			posted = append(posted, r.URL.Path)
			mu.Unlock()
			rec := httptest.NewRecorder()
			ch.HandleWebhook(rec, signed906("guid-906", payload))
			if rec.Code != http.StatusOK {
				t.Errorf("replayed delivery = %d %s", rec.Code, rec.Body.String())
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gh.Close()
	ch, err = github.New(github.Config{WebhookSecret: "shhh906", APIBaseURL: gh.URL, HTTPClient: gh.Client(),
		Installations: []github.InstallationConfig{{ProjectID: "auto894", InstallationID: 906, AppID: 9060,
			PrivateKey: key, RepoAllowlist: []string{"acme/api"}, TaskLabels: []string{"vornik-task"}, TaskCreator: tc}}})
	if err != nil {
		t.Fatal(err)
	}
	h.c.githubChannel.Store(ch)

	// While held the delivery is refused before any handler.
	if w := h.do(http.MethodPost, "/api/v1/github-app/webhook", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("webhook while held = %d, want 503", w.Code)
	}

	w := h.do(http.MethodDelete, "/api/v1/daemon/hold", held586Key)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"redelivery":"started"`) {
		t.Fatalf("release = %d %s", w.Code, w.Body.String())
	}
	waitFor586(t, 10*time.Second, "the pass's report on GET", func() bool {
		return strings.Contains(h.do(http.MethodGet, "/api/v1/daemon/hold", held586Key).Body.String(), `"state":"done"`)
	})
	g := h.do(http.MethodGet, "/api/v1/daemon/hold", held586Key).Body.String()
	if !strings.Contains(g, `"redelivered":1`) || !strings.Contains(g, `"not_redelivered":0`) {
		t.Fatalf("GET after the pass = %s, want one redelivered, none left", g)
	}
	mu.Lock()
	n := len(posted)
	mu.Unlock()
	if n != 1 || tasks() != 1 {
		t.Fatalf("redelivery POSTs = %d, tasks with the delivery's key = %d; want 1 and 1", n, tasks())
	}
	// GitHub sending the redelivery twice still makes one task.
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signed906("guid-906", payload))
	if all := countTasks906(t, h); all != 2 {
		t.Fatalf("tasks in auto894 = %d after a repeated redelivery, want 2 (t586 and the delivery's)", all)
	}
}

var project906 = "auto894"

func countTasks906(t *testing.T, h *held586) int {
	t.Helper()
	ts, err := h.c.repos.Tasks.List(context.Background(), persistence.TaskFilter{ProjectID: &project906})
	if err != nil {
		t.Fatal(err)
	}
	return len(ts)
}
