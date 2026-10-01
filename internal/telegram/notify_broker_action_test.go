package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Broker write-actions design 2026-09-29 §5.3: Telegram is notify-only. The
// alert names the project, the task and how many writes wait, links to
// /inbox, and carries no arguments and no inline decision.
func TestBuildBrokerActionCaption(t *testing.T) {
	c := buildBrokerActionCaption("broker-mail", "task_42", 2, "https://vornik.example/ui/inbox")
	for _, want := range []string{"broker-mail", "task_42", "2 ", "https://vornik.example/ui/inbox"} {
		if !strings.Contains(c, want) {
			t.Errorf("caption missing %q:\n%s", want, c)
		}
	}
	lower := strings.ToLower(c)
	for _, forbidden := range []string{"approve", "reject", "callback", "/approve", "/reject"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("notify-only caption must not contain %q:\n%s", forbidden, c)
		}
	}
	if one := buildBrokerActionCaption("p", "t", 1, "u"); !strings.Contains(one, "1 broker write ") {
		t.Errorf("singular form:\n%s", one)
	}
}

func TestNotifyBrokerActionsPending_TextToEveryOperatorWithoutMarkup(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()
	b := stubBot(srv.URL, 1, 2)
	if err := b.NotifyBrokerActionsPending(context.Background(), "broker-mail", "task_42", 1, "https://v/ui/inbox"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("sends = %d, want one per operator", len(bodies))
	}
	for _, body := range bodies {
		if strings.Contains(body, "reply_markup") || strings.Contains(body, "callback_data") {
			t.Fatalf("notify-only: markup in %s", body)
		}
	}
}
