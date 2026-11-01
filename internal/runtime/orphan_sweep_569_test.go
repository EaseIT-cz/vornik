package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #569 R4 (08-runtime-manager-podman.md, "Change 2026-10-28"): the
// orphan sweep removes, volumes included, only what no one can still need: an
// exited container of a terminal task, and a running one whose task has been
// terminal for more than 10 minutes. It keeps every container of a live task
// (recovery adopts an exited one), of a task this database does not know
// (another daemon's), of a failed lookup, and every warm container.

type sweepPodman569 struct {
	mu      sync.Mutex
	ps      []podmanPsData
	removed []string
}

func (s *sweepPodman569) exec(_ context.Context, args []string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch args[0] {
	case "ps":
		b, _ := json.Marshal(s.ps)
		return b, nil
	case "rm":
		s.removed = append(s.removed, strings.Join(args, " "))
		return nil, nil
	}
	return nil, errors.New("unexpected verb " + args[0])
}

func psRow569(name, state, taskID string) podmanPsData {
	labels := map[string]string{LabelManaged: LabelValueTrue, LabelProjectID: "p", LabelRole: "coder"}
	if taskID != "" {
		labels[LabelTaskID] = taskID
	}
	return podmanPsData{Id: name, Names: []string{name}, State: state, Labels: labels}
}

func TestOrphanSweep569_Rules(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	tasks := map[string]OrphanTask{
		"t-done":      {Found: true, Terminal: true, UpdatedAt: now.Add(-time.Hour)},
		"t-live":      {Found: true, Terminal: false},
		"t-runaway":   {Found: true, Terminal: true, UpdatedAt: now.Add(-11 * time.Minute)},
		"t-justended": {Found: true, Terminal: true, UpdatedAt: now.Add(-5 * time.Minute)},
	}
	fake := &sweepPodman569{ps: []podmanPsData{
		psRow569("c-done", "exited", "t-done"),
		psRow569("c-live-exited", "exited", "t-live"),
		psRow569("c-live-running", "running", "t-live"),
		psRow569("c-runaway", "running", "t-runaway"),
		psRow569("c-justended", "running", "t-justended"),
		psRow569("c-unknown", "exited", "t-other-daemon"),
		psRow569("c-lookupfail", "exited", "t-fail"),
		psRow569("c-warm", "running", "warm-1793193512283988401"),
		psRow569("c-unlabelled", "exited", ""),
	}}
	m := &Manager{podmanPath: "/bin/false", logger: zerolog.Nop(), defaultTimeout: time.Minute, execPodman: fake.exec}
	var looked []string
	lookup := func(_ context.Context, id string) (OrphanTask, error) {
		looked = append(looked, id)
		if id == "t-fail" {
			return OrphanTask{}, errors.New("store not ready")
		}
		return tasks[id], nil // absent → Found false
	}
	got := m.SweepOrphans(context.Background(), lookup, now)
	slices.Sort(fake.removed)
	want := []string{"rm --force --volumes c-done", "rm --force --volumes c-runaway"}
	if !slices.Equal(fake.removed, want) {
		t.Errorf("removed %v, want %v", fake.removed, want)
	}
	if slices.Contains(looked, "warm-1793193512283988401") {
		t.Error("the sweep looked up a warm container's task id")
	}
	wantCounts := map[string]int{
		SweepRemovedExited: 1, SweepRemovedRunaway: 1, SweepKeptLive: 3,
		SweepKeptUnknown: 1, SweepKeptLookupFailed: 1,
	}
	for rule, n := range wantCounts {
		if got[rule] != n {
			t.Errorf("%s = %d, want %d (all %v)", rule, got[rule], n, got)
		}
	}
}

// A kept container is warned about once per daemon process, and the warned
// set forgets a container once it is gone.
func TestOrphanSweep569_WarnsOnce(t *testing.T) {
	fake := &sweepPodman569{ps: []podmanPsData{psRow569("c-unknown", "exited", "t-other")}}
	m := &Manager{podmanPath: "/bin/false", logger: zerolog.Nop(), defaultTimeout: time.Minute, execPodman: fake.exec}
	lookup := func(context.Context, string) (OrphanTask, error) { return OrphanTask{}, nil }
	now := time.Now()
	m.SweepOrphans(context.Background(), lookup, now)
	if !m.sweepWarnedOnce("c-unknown") {
		t.Fatal("the unknown container was not recorded as warned")
	}
	fake.ps = nil
	m.SweepOrphans(context.Background(), lookup, now)
	if m.sweepWarnedOnce("c-unknown") {
		t.Error("the warned set kept a container that is gone")
	}
}
