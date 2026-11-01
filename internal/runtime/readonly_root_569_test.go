package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #569 (08-runtime-manager-podman.md, "Change 2026-10-28"): with the
// read-only root on, an agent container gets --read-only, podman's own tmpfs
// turned off, bounded /run and /dev/shm tmpfs and three disk-backed anonymous
// volumes; with it off the run argv is today's. Every removal takes the
// container's anonymous volumes with it, and every start attempt first removes
// a same-named container, volumes included, so --replace never leaks them.

type callRecorder569 struct {
	mu    sync.Mutex
	calls [][]string
	// runErrs are returned, in order, for successive "run" calls.
	runErrs []string
	rmErr   string
}

func (r *callRecorder569) exec(_ context.Context, args []string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	switch args[0] {
	case "run":
		if len(r.runErrs) > 0 {
			msg := r.runErrs[0]
			r.runErrs = r.runErrs[1:]
			return []byte(msg), errors.New("exit status 125")
		}
		return []byte("cid-569\n"), nil
	case "rm":
		if r.rmErr != "" {
			return []byte(r.rmErr), errors.New("exit status 2")
		}
		return []byte("no such container\n"), errors.New("exit status 1")
	}
	return []byte("[]"), nil
}

func (r *callRecorder569) verbs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func manager569(rec *callRecorder569, opts ...ManagerOption) *Manager {
	m := &Manager{
		podmanPath:     "/bin/false",
		logger:         zerolog.Nop(),
		defaultTimeout: 60 * time.Second,
		imageTimeout:   15 * time.Minute,
		execPodman:     rec.exec,
		imageLabelFunc: func(context.Context, string) (string, bool, error) { return "id", false, nil },
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func config569() *ContainerConfig {
	return &ContainerConfig{Image: "ghcr.io/easeit-cz/vornik-agent:latest", ProjectID: "p", Role: "coder", TaskID: "task_569"}
}

var hardeningTokens569 = []string{"--read-only", "--read-only-tmpfs=false", "--tmpfs", "--pids-limit"}

func volumeDestinations569(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--volume" && !strings.Contains(args[i+1], ":") {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestRuntime569_OffArgvIsToday(t *testing.T) {
	m := manager569(&callRecorder569{})
	args := m.buildPreImageArgs(config569())
	for _, tok := range hardeningTokens569 {
		if slices.Contains(args, tok) {
			t.Errorf("settings off, the argv carries %s: %v", tok, args)
		}
	}
	if v := volumeDestinations569(args); len(v) != 0 {
		t.Errorf("settings off, anonymous volumes %v", v)
	}
}

func TestRuntime569_OnArgv(t *testing.T) {
	m := manager569(&callRecorder569{}, WithAgentReadOnlyRoot(true), WithAgentPidsLimit(2048))
	args := m.buildPreImageArgs(config569())
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--read-only", "--read-only-tmpfs=false",
		"--tmpfs /run:rw,size=16m,mode=1777", "--tmpfs /dev/shm:rw,size=64m,mode=1777",
		"--pids-limit 2048",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("on, the argv lacks %q: %s", want, joined)
		}
	}
	if got := volumeDestinations569(args); !slices.Equal(got, []string{"/tmp", "/var/tmp", "/home/vornik"}) {
		t.Errorf("anonymous volumes %v, want /tmp, /var/tmp, /home/vornik", got)
	}
	// A PID limit alone adds only --pids-limit.
	p := manager569(&callRecorder569{}, WithAgentPidsLimit(512)).buildPreImageArgs(config569())
	if !strings.Contains(strings.Join(p, " "), "--pids-limit 512") || slices.Contains(p, "--read-only") {
		t.Errorf("pids only: %v", p)
	}
}

func TestRuntime569_RemoveTakesVolumes(t *testing.T) {
	rec := &callRecorder569{}
	_ = manager569(rec).RemoveContainer(context.Background(), "c1", true)
	if got := rec.verbs(); len(got) != 1 || got[0] != "rm --force --volumes c1" {
		t.Errorf("RemoveContainer: %v", got)
	}
}

// Every start attempt, the userns chain's second included, is preceded by a
// removal of the same-named container with its volumes.
func TestRuntime569_EveryAttemptRemovesTheSameNamedContainerFirst(t *testing.T) {
	rec := &callRecorder569{runErrs: []string{"Error: cannot set up namespace using newuidmap"}}
	m := manager569(rec)
	if _, err := m.StartContainer(context.Background(), config569()); err != nil {
		t.Fatalf("start: %v", err)
	}
	name := ContainerName("p", "coder", "task_569")
	var seq []string
	for _, c := range rec.verbs() {
		switch {
		case strings.HasPrefix(c, "rm "):
			seq = append(seq, c)
		case strings.HasPrefix(c, "run "):
			seq = append(seq, "run")
		}
	}
	want := []string{"rm --force --volumes " + name, "run", "rm --force --volumes " + name, "run"}
	if !slices.Equal(seq, want) {
		t.Errorf("calls %v, want %v", seq, want)
	}
}

// A pre-start removal that fails for any reason but "no such container"
// aborts the attempt, rather than letting --replace leak the volumes.
func TestRuntime569_FailedPreStartRemovalAborts(t *testing.T) {
	rec := &callRecorder569{rmErr: "Error: container state improper"}
	m := manager569(rec)
	if _, err := m.StartContainer(context.Background(), config569()); err == nil {
		t.Fatal("started after a failed pre-start removal")
	}
	for _, c := range rec.verbs() {
		if strings.HasPrefix(c, "run ") {
			t.Fatalf("podman run was called after the removal failed: %v", rec.verbs())
		}
	}
}

// The third attempt of the userns chain (host, when allowed) is preceded by
// its own removal too (review-20261028-91b7 #5).
func TestRuntime569_ThirdAttemptRemovesFirst(t *testing.T) {
	ns := "Error: cannot set up namespace using newuidmap"
	rec := &callRecorder569{runErrs: []string{ns, ns}}
	m := manager569(rec)
	m.allowHostUserns = true
	if _, err := m.StartContainer(context.Background(), config569()); err != nil {
		t.Fatalf("start: %v", err)
	}
	var rms, runs int
	for _, c := range rec.verbs() {
		switch {
		case strings.HasPrefix(c, "rm --force --volumes "):
			rms++
		case strings.HasPrefix(c, "run "):
			runs++
		}
	}
	if runs != 3 || rms != 3 {
		t.Errorf("%d runs, %d pre-start removals; want 3 and 3: %v", runs, rms, rec.verbs())
	}
}

// The pool's warm containers carry a warm- task id, the label the orphan
// sweep skips before any lookup (review-20261028-91b7 #3).
func TestRuntime569_WarmContainerCarriesTheWarmTaskID(t *testing.T) {
	rec := &callRecorder569{}
	m := manager569(rec)
	pool := newS6WarmPool(m, t.TempDir())
	entry, err := pool.StartWarm(context.Background(), PoolKey{ProjectID: "p1", Role: "coder", Image: "localhost/vornik-agent:test"}, nil)
	if err != nil {
		t.Fatalf("StartWarm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(entry.InputDir)) })
	for _, c := range rec.verbs() {
		if strings.HasPrefix(c, "run ") {
			if !strings.Contains(c, "--label "+LabelTaskID+"="+warmTaskPrefix) {
				t.Errorf("a warm container's run carries no %s=%s label: %s", LabelTaskID, warmTaskPrefix, c)
			}
			return
		}
	}
	t.Fatal("StartWarm ran no container")
}
