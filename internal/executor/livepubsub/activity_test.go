package livepubsub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #119 item 9 (2026-10-08): progress while a task runs. status reads
// the running step's start and the newest event from a per-execution memo
// (live-task-observation design, "Change 2026-10-08: progress while a task
// runs" G3) instead of replaying the ring or the live_events table.

func activityOf(t *testing.T, p Publisher, execID string) (Activity, bool) {
	t.Helper()
	r, ok := p.(ActivityReader)
	if !ok {
		t.Fatalf("%T does not implement ActivityReader", p)
	}
	return r.Activity(execID)
}

func TestActivity_TracksStepAndLastEvent(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	p.Publish(ctx, "exec_a", KindStepStarted, StepStartedPayload{StepID: "read"})
	a1, ok := activityOf(t, p, "exec_a")
	if !ok || a1.StepID != "read" || a1.StepStartedAt.IsZero() {
		t.Fatalf("after step_started: %+v ok=%v", a1, ok)
	}
	if !a1.LastLifecycleAt.Equal(a1.StepStartedAt) || !a1.LastEventAt.Equal(a1.StepStartedAt) {
		t.Fatalf("step_started is a lifecycle event and an event: %+v", a1)
	}

	time.Sleep(2 * time.Millisecond)
	p.Publish(ctx, "exec_a", KindToolCallStarted, map[string]any{"tool": "x"})
	a2, _ := activityOf(t, p, "exec_a")
	if !a2.LastEventAt.After(a1.LastEventAt) {
		t.Fatalf("a tool call moves LastEventAt: %+v", a2)
	}
	if !a2.LastLifecycleAt.Equal(a1.LastLifecycleAt) {
		t.Fatalf("a tool call must not move LastLifecycleAt (the broker view drops it): %+v", a2)
	}
	if a2.StepID != "read" || !a2.StepStartedAt.Equal(a1.StepStartedAt) {
		t.Fatalf("a tool call must not change the step: %+v", a2)
	}

	time.Sleep(2 * time.Millisecond)
	p.Publish(ctx, "exec_a", KindStepCompleted, StepCompletedPayload{StepID: "read"})
	p.Publish(ctx, "exec_a", KindStepStarted, &StepStartedPayload{StepID: "curate"})
	a3, _ := activityOf(t, p, "exec_a")
	if a3.StepID != "curate" || !a3.StepStartedAt.After(a1.StepStartedAt) {
		t.Fatalf("a second step_started (pointer payload) moves the step: %+v", a3)
	}
	if !a3.LastLifecycleAt.Equal(a3.StepStartedAt) {
		t.Fatalf("lifecycle moves with the second step: %+v", a3)
	}
}

func TestActivity_UnknownExecution(t *testing.T) {
	p := New(10)
	if a, ok := activityOf(t, p, "exec_none"); ok || a != (Activity{}) {
		t.Fatalf("unknown execution: %+v ok=%v", a, ok)
	}
	// Reading must not create a stream (so a poller cannot pin memory).
	ip := p.(*inProcessPublisher)
	ip.mu.Lock()
	n := len(ip.streams)
	ip.mu.Unlock()
	if n != 0 {
		t.Fatalf("Activity created %d stream(s)", n)
	}
}

func TestActivity_RemoteIngestUpdates(t *testing.T) {
	repo := newFakeLiveEventRepo()
	listener := newFakeListener()
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo: repo, Notifier: newFakeNotifier(), Listener: listener, NodeID: "node-a", Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	// Another replica persisted a step_started and notified: the payload
	// arrives as decoded JSON (a map), not the struct.
	pl, _ := json.Marshal(StepStartedPayload{StepID: "from-b"})
	seq, _ := repo.Append(context.Background(), "exec-x", KindStepStarted, pl)
	listener.ch <- Notification{Channel: NotifyChannel, Payload: "exec-x|" + itoa(seq) + "|node-b"}

	deadline := time.Now().Add(2 * time.Second)
	for {
		a, ok := activityOf(t, pub, "exec-x")
		if ok && a.StepID == "from-b" && !a.StepStartedAt.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote step_started never reached the memo: %+v ok=%v", a, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
