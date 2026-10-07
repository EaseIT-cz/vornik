package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

type liveControlSubscriber struct {
	livepubsub.Publisher
	subscribed   chan struct{}
	unsubscribed chan struct{}
}

func (s *liveControlSubscriber) Subscribe(id string, seq int64) (<-chan livepubsub.LiveEvent, func(), error) {
	ch, unsub, err := s.Publisher.Subscribe(id, seq)
	close(s.subscribed)
	var once sync.Once
	return ch, func() { once.Do(func() { unsub(); close(s.unsubscribed) }) }, err
}

func liveControlConnection(t *testing.T) (*websocket.Conn, *liveControlSubscriber) {
	t.Helper()
	sub := &liveControlSubscriber{Publisher: livepubsub.New(10), subscribed: make(chan struct{}), unsubscribed: make(chan struct{})}
	srv := newLiveServer(t, sub, &persistence.Execution{ID: "exec_1", ProjectID: "p1"})
	serverCtx, serverCancel := context.WithCancel(context.Background())
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ExecutionLive(w, r.WithContext(serverCtx), "exec_1")
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		serverCancel()
		hs.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { serverCancel(); _ = conn.CloseNow(); hs.Close() })
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"last_seq":0}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.subscribed:
	case <-ctx.Done():
		t.Fatal("hello did not subscribe")
	}
	return conn, sub
}

// Both library Ping and browser pong processing require the server to keep
// reading control frames after its initial cursor hello. This real upgraded
// connection exposes the production heartbeat defect without waiting 30s.
func TestExecutionLive_ControlFramesAndEvents(t *testing.T) {
	conn, sub := liveControlConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := make(chan livepubsub.LiveEvent, 1)
	readErr := make(chan error, 1)
	go func() {
		_, body, err := conn.Read(ctx)
		if err != nil {
			readErr <- err
			return
		}
		var evt livepubsub.LiveEvent
		if err := json.Unmarshal(body, &evt); err != nil {
			readErr <- err
			return
		}
		events <- evt
	}()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("healthy client ping was not answered after hello: %v", err)
	}
	sub.Publish(ctx, "exec_1", "llm_call_finished", map[string]int{"tokens": 42})
	select {
	case evt := <-events:
		if evt.Kind != "llm_call_finished" {
			t.Fatalf("unexpected event: %+v", evt)
		}
	case err := <-readErr:
		t.Fatalf("event read failed: %v", err)
	case <-ctx.Done():
		t.Fatal("event did not arrive after heartbeat")
	}
	_ = conn.CloseNow()
	select {
	case <-sub.unsubscribed:
	case <-ctx.Done():
		t.Fatal("client disconnect did not promptly release subscription")
	}
}

func TestExecutionLive_RejectsDataAfterHello(t *testing.T) {
	conn, sub := liveControlConnection(t)
	// The installed library bounds its policy-close goroutine teardown at 15s.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"last_seq":123}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("second application message close=%v; want policy violation (err=%v)", got, err)
	}
	// Finish transport teardown rather than waiting for the library's bounded
	// policy-close handshake while the rejected message body is unread.
	_ = conn.CloseNow()
	select {
	case <-sub.unsubscribed:
	case <-ctx.Done():
		t.Fatal("policy refusal did not release subscription")
	}
}

// Exercise the production server Ping interval and pong wait without injecting
// shorter timers: an event after 32s arrives beyond the pre-fix disconnect.
func TestExecutionLive_SurvivesProductionHeartbeat(t *testing.T) {
	conn, sub := liveControlConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 38*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, body, err := conn.Read(ctx)
		if err == nil {
			var evt livepubsub.LiveEvent
			err = json.Unmarshal(body, &evt)
			if err == nil && evt.Kind != "llm_call_finished" {
				err = fmt.Errorf("unexpected event: %s", evt.Kind)
			}
		}
		result <- err
	}()
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		t.Fatalf("healthy live stream ended before first heartbeat completed: %v", err)
	case <-timer.C:
	}
	sub.Publish(ctx, "exec_1", "llm_call_finished", map[string]int{"tokens": 42})
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("event after production heartbeat: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("event after production heartbeat timed out")
	}
}
