package cli

// GitHub #915 (benchmark LLD §12.30, amendment 2026-11-25): a learning arm's
// healing loop replays a candidate as a task in the benchmark project, on the
// benchmark workspace, in parallel with the runner. On 2026-11-24 (explore3
// arm C) the runner's next repeat found the replay's live worktree and
// refused, failing the chunk 38 runs into an 84-run warm-up. A live worktree
// of a running task is now waited for; a leftover is still refused at once.
//
// Found at: production (the operator's explore3 study, 2026-11-24), observed
// at e2e-lane (the bench runner's Prepare between repeats).
// Lower because: Prepare runs here on a real git repository exactly as the
// runner runs it; the daemon's status answer is faked, which is the seam.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentbench"
)

// fakeStatuses answers the daemon's task status per id, in turn per call;
// the last answer repeats.
type fakeStatuses struct {
	mu      sync.Mutex
	answers map[string][]statusAnswer
	calls   map[string]int
}

type statusAnswer struct {
	status string
	found  bool
	err    error
}

func (f *fakeStatuses) lookup(_ context.Context, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	as := f.answers[id]
	if len(as) == 0 {
		return "", false, nil
	}
	i := f.calls[id]
	f.calls[id]++
	if i >= len(as) {
		i = len(as) - 1
	}
	return as[i].status, as[i].found, as[i].err
}

func liveWorktreeWorkspace(t *testing.T, tasks ...string) (dir string, wts []string) {
	t.Helper()
	dir, _ = historyWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range tasks {
		wt := filepath.Join(dir, ".worktrees", id)
		gitOut(t, dir, "worktree", "add", "-q", wt, "-b", "worktree/"+id)
		wts = append(wts, wt)
	}
	return dir, wts
}

// later runs git steps on a timer off the test goroutine and reports a
// failure through the returned channel, never with t.Fatal there.
func later(steps ...func() error) <-chan error {
	done := make(chan error, 1)
	go func() {
		for _, step := range steps {
			if err := step(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return done
}

func sleep(d time.Duration) func() error { return func() error { time.Sleep(d); return nil } }

func gitStep(dir string, args ...string) func() error {
	return func() error {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			return errors.New(string(out))
		}
		return nil
	}
}

func checkLater(t *testing.T, done <-chan error) {
	t.Helper()
	if err := <-done; err != nil {
		t.Fatalf("background git step: %v", err)
	}
}

// fastWorkspace polls fast. Its bound is a hang guard, not a measurement:
// each passing case returns as soon as its worktree goes, and the one case
// that exercises the bound sets its own. 2 s failed under a loaded local
// gate (2026-11-25: git worktree add plus remove took longer).
func fastWorkspace(dir string, f *fakeStatuses) gitWorkspace {
	return gitWorkspace{dir: dir, taskStatus: f.lookup, waitBound: 30 * time.Second, pollEvery: 20 * time.Millisecond}
}

func prepare915(w gitWorkspace) error {
	_, err := w.Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1)
	return err
}

func TestBenchWorkspace915_AReplaysWorktreeIsWaitedFor(t *testing.T) {
	dir, wts := liveWorktreeWorkspace(t, "task_replay")
	f := &fakeStatuses{answers: map[string][]statusAnswer{"task_replay": {{status: "RUNNING", found: true}}}}
	w := fastWorkspace(dir, f)
	done := later(sleep(100*time.Millisecond), gitStep(dir, "worktree", "remove", "--force", wts[0]))
	if err := prepare915(w); err != nil {
		t.Fatalf("a running task's worktree that goes away must let Prepare proceed: %v", err)
	}
	checkLater(t, done)
}

func TestBenchWorkspace915_AnUnreadableStatusIsRetried(t *testing.T) {
	dir, wts := liveWorktreeWorkspace(t, "task_replay")
	f := &fakeStatuses{answers: map[string][]statusAnswer{"task_replay": {
		{err: errors.New("dial tcp: connection refused")}, {err: errors.New("500")}, {status: "RUNNING", found: true},
	}}}
	w := fastWorkspace(dir, f)
	done := later(sleep(150*time.Millisecond), gitStep(dir, "worktree", "remove", "--force", wts[0]))
	if err := prepare915(w); err != nil {
		t.Fatalf("a lookup that fails twice and then answers RUNNING must be retried: %v", err)
	}
	checkLater(t, done)
}

func TestBenchWorkspace915_AWorktreeAppearingMidWaitIsWaitedForToo(t *testing.T) {
	dir, wts := liveWorktreeWorkspace(t, "task_one")
	f := &fakeStatuses{answers: map[string][]statusAnswer{
		"task_one": {{status: "RUNNING", found: true}},
		"task_two": {{status: "QUEUED", found: true}},
	}}
	w := fastWorkspace(dir, f)
	second := filepath.Join(dir, ".worktrees", "task_two")
	done := later(sleep(60*time.Millisecond),
		gitStep(dir, "worktree", "add", "-q", second, "-b", "worktree/task_two"),
		gitStep(dir, "worktree", "remove", "--force", wts[0]),
		sleep(150*time.Millisecond),
		gitStep(dir, "worktree", "remove", "--force", second))
	if err := prepare915(w); err != nil {
		t.Fatalf("a second live worktree must be waited for, then Prepare proceeds: %v", err)
	}
	checkLater(t, done)
}

func TestBenchWorkspace915_ALeftoverIsRefusedAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer statusAnswer
	}{
		{"COMPLETED", statusAnswer{status: "COMPLETED", found: true}},
		{"CLOSED", statusAnswer{status: "CLOSED", found: true}},
		{"FAILED", statusAnswer{status: "FAILED", found: true}},
		{"unknown to the daemon", statusAnswer{found: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, wts := liveWorktreeWorkspace(t, "task_old")
			f := &fakeStatuses{answers: map[string][]statusAnswer{"task_old": {tc.answer}}}
			w := fastWorkspace(dir, f)
			w.waitBound = time.Hour // refused at once, never by the bound
			start := time.Now()
			err := prepare915(w)
			if err == nil || !strings.Contains(err.Error(), wts[0]) {
				t.Fatalf("a leftover must refuse and be named: %v", err)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("a leftover must be refused at once, took %v", time.Since(start))
			}
		})
	}
}

func TestBenchWorkspace915_AnUnidentifiableWorktreeIsRefused(t *testing.T) {
	dir, _ := historyWorkspace(t)
	odd := filepath.Join(t.TempDir(), "elsewhere")
	gitOut(t, dir, "worktree", "add", "-q", odd, "-b", "hand-made")
	w := fastWorkspace(dir, &fakeStatuses{})
	err := prepare915(w)
	if err == nil || !strings.Contains(err.Error(), "not a task worktree") || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("a worktree outside .worktrees/<task id> must be refused with its own message: %v", err)
	}
}

func TestBenchWorkspace915_TheWaitIsBounded(t *testing.T) {
	dir, _ := liveWorktreeWorkspace(t, "task_slow")
	f := &fakeStatuses{answers: map[string][]statusAnswer{"task_slow": {{status: "RUNNING", found: true}}}}
	w := fastWorkspace(dir, f)
	w.waitBound = 150 * time.Millisecond
	err := prepare915(w)
	if err == nil || !strings.Contains(err.Error(), "task_slow") || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("a task still live at the bound must refuse, naming it: %v", err)
	}
}

func TestBenchWorkspace915_TheResetRechecksBeforeItDeletes(t *testing.T) {
	dir, oldHead := historyWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := fastWorkspace(dir, &fakeStatuses{})
	late := filepath.Join(dir, ".worktrees", "task_late")
	// A replay creates its worktree after the wait found none, before the reset.
	w.afterWait = func() { gitOut(t, dir, "worktree", "add", "-q", late, "-b", "worktree/task_late") }
	err := prepare915(w)
	if err == nil || !strings.Contains(err.Error(), late) {
		t.Fatalf("the reset must re-check and refuse a worktree that appeared after the wait: %v", err)
	}
	if out := gitOut(t, dir, "rev-parse", "--verify", "worktree/task_late"); out == "" {
		t.Fatal("the late worktree's branch must be left intact")
	}
	if err := exec.Command("git", "-C", dir, "cat-file", "-e", oldHead).Run(); err != nil {
		t.Fatal("a refused reset leaves the history untouched")
	}
}

func TestBenchWorkspace915_WithoutALookupTheRefusalIsImmediate(t *testing.T) {
	dir, wts := liveWorktreeWorkspace(t, "task_x")
	err := prepare915(gitWorkspace{dir: dir})
	if err == nil || !strings.Contains(err.Error(), wts[0]) {
		t.Fatalf("without a daemon lookup a live worktree is refused as before: %v", err)
	}
}

func TestBenchTaskStatus915_ReadsTheDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/agentbench/tasks/task_live":
			_, _ = w.Write([]byte(`{"id":"task_live","status":"RUNNING"}`))
		case "/api/v1/projects/agentbench/tasks/task_odd":
			_, _ = w.Write([]byte(`{"id":"task_odd"}`))
		case "/api/v1/projects/agentbench/tasks/task_boom":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "k")
	old := benchAgentProject
	benchAgentProject = "agentbench"
	defer func() { benchAgentProject = old }()

	if st, found, err := benchTaskStatus(context.Background(), "task_live"); err != nil || !found || st != "RUNNING" {
		t.Fatalf("live: %q %v %v", st, found, err)
	}
	if _, found, err := benchTaskStatus(context.Background(), "task_gone"); err != nil || found {
		t.Fatalf("a 404 is not found, without error: %v %v", found, err)
	}
	if _, _, err := benchTaskStatus(context.Background(), "task_boom"); err == nil {
		t.Fatal("a 500 is an unreadable status")
	}
	if _, _, err := benchTaskStatus(context.Background(), "task_odd"); err == nil {
		t.Fatal("an answer with no status is unreadable")
	}
	benchAgentProject = ""
	if _, _, err := benchTaskStatus(context.Background(), "task_live"); err == nil {
		t.Fatal("no project is an error")
	}
}

func TestBenchWorkspace915_ARelativeWorkspaceStillKnowsItsTaskWorktrees(t *testing.T) {
	dir, wts := liveWorktreeWorkspace(t, "task_replay")
	t.Chdir(filepath.Dir(dir))
	f := &fakeStatuses{answers: map[string][]statusAnswer{"task_replay": {{status: "RUNNING", found: true}}}}
	w := fastWorkspace(filepath.Base(dir), f)
	done := later(sleep(100*time.Millisecond), gitStep(dir, "worktree", "remove", "--force", wts[0]))
	if err := prepare915(w); err != nil {
		t.Fatalf("a relative --workspace must still recognise .worktrees/<task id> and wait: %v", err)
	}
	checkLater(t, done)
}

// Gate flake 2026-11-25: prune raced a replay's last worktree removal and
// failed with "Invalid path .../.git/worktrees". retryGitRace retries it.
func TestRetryGitRace_RetriesAFailureThenSucceeds(t *testing.T) {
	calls := 0
	err := retryGitRace(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls == 1 {
			return errors.New("fatal: Invalid path '.git/worktrees': No such file or directory")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want nil after 2", err, calls)
	}
	calls = 0
	err = retryGitRace(context.Background(), 3, time.Millisecond, func() error { calls++; return errors.New("still broken") })
	if err == nil || calls != 3 {
		t.Fatalf("a lasting failure: err=%v calls=%d, want the error after 3", err, calls)
	}
}
