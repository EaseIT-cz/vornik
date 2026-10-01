package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type idRecordingNotifier struct{ ids []string }

func (r *idRecordingNotifier) NotifyTaskCompleted(_ context.Context, task *persistence.Task, _ bool, _ string) {
	r.ids = append(r.ids, task.ID)
}

// Broker design §6: the companion's result(wait_seconds) long-poll is released
// by the executor's terminal transitions. The completion NOTIFIER is replaced
// wholesale by whichever chat subsystem starts last (and is absent on a daemon
// with no chat channel), so the waiter rides a separate observer list that
// fires whether or not a notifier is configured.
func TestNotifyCompletion_ObserversFireWithoutANotifier(t *testing.T) {
	e := &Executor{}
	obs := &idRecordingNotifier{}
	e.AddCompletionObserver(obs)
	e.notifyCompletion(context.Background(), &persistence.Task{ID: "t1"}, true, "ok", true)
	if len(obs.ids) != 1 || obs.ids[0] != "t1" {
		t.Fatalf("observer calls = %v, want [t1]", obs.ids)
	}
	notifier := &idRecordingNotifier{}
	e.SetCompletionNotifier(notifier)
	e.notifyCompletion(context.Background(), &persistence.Task{ID: "t2"}, false, "x", false)
	if len(notifier.ids) != 1 || len(obs.ids) != 2 {
		t.Fatalf("notifier=%v observer=%v", notifier.ids, obs.ids)
	}
	var nilExec *Executor
	nilExec.AddCompletionObserver(obs) // must not panic
}

// Every terminal notification must go through notifyCompletion, or an
// observer silently misses that path.
func TestNoDirectCompletionNotifierCalls(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	direct := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		direct += strings.Count(string(b), "e.notifier.NotifyTaskCompleted(")
	}
	// Exactly one: the call inside notifyCompletion itself.
	if direct != 1 {
		t.Errorf("found %d direct e.notifier.NotifyTaskCompleted calls, want 1 (inside notifyCompletion); route the others through e.notifyCompletion", direct)
	}
}

func TestOriginalArtifactName(t *testing.T) {
	cases := map[string]string{
		"digest-20260929-ab12.json":  "digest.json",
		"digest.json":                "digest.json",
		"raw_mail-20260929-ab12.txt": "raw_mail.txt",
	}
	for in, want := range cases {
		if got := OriginalArtifactName(in); got != want {
			t.Errorf("OriginalArtifactName(%q) = %q, want %q", in, got, want)
		}
	}
}
