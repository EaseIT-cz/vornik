package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// GitHub #428 (found at: review; observed by an operator running
// `vornikctl deps install`): the verb's probe and pip containers ran with no
// memory bound and no swap bound. They now carry the agents' own limit,
// resolved from the daemon's config by the daemon's rule, with no swap
// (project dependency provisioning design §8.6).

func TestDepsInstall_ContainersCarryTheAgentMemoryLimit(t *testing.T) {
	in := depsFixture(t)
	in.MemoryLimit = 1 << 30
	in.MemoryNote = "memory limit 1.0 GiB, no swap (runtime.agent_memory_limit)"
	f := goodRunner()
	var out bytes.Buffer
	if err := runDepsInstall(context.Background(), in, f.run, &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), in.MemoryNote) {
		t.Errorf("the verb must print the bound before it runs anything:\n%s", out.String())
	}
	probes, pips := 0, 0
	for _, c := range f.calls {
		if c[0] != "run" {
			continue
		}
		j := " " + strings.Join(c, " ") + " "
		if !strings.Contains(j, " --memory=1073741824 ") || !strings.Contains(j, " --memory-swap=1073741824 ") {
			t.Errorf("every install container must carry the agent limit with no swap: %v", c)
		}
		switch {
		case strings.Contains(j, " pip install "):
			pips++
		case strings.Contains(j, " -c "):
			probes++
		}
	}
	if probes != 1 || pips != 1 {
		t.Fatalf("want one probe and one pip run, got %d and %d: %v", probes, pips, f.calls)
	}

	unbounded := depsFixture(t)
	unbounded.MemoryNote = `memory limit: none (runtime.agent_memory_limit is "none")`
	g := goodRunner()
	out.Reset()
	if err := runDepsInstall(context.Background(), unbounded, g.run, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.calls {
		if strings.Contains(strings.Join(c, " "), "--memory") {
			t.Errorf(`"none" must emit no memory flag: %v`, c)
		}
	}
}

func TestDepsMemoryLimit_TheDaemonsRule(t *testing.T) {
	const gib = int64(1) << 30
	total := func(n int64, err error) func() (int64, error) { return func() (int64, error) { return n, err } }

	// Absent derives, as the daemon does: 8 GiB x 0.5 / 4 = 1 GiB.
	got, note, err := depsMemoryLimit("", 4, total(8*gib, nil))
	if err != nil || got != gib || !strings.Contains(note, "1.0 GiB") || !strings.Contains(note, "derived") || !strings.Contains(note, "no swap") {
		t.Errorf(`"" = %d %q %v, want 1 GiB derived`, got, note, err)
	}
	// An explicit size is used as written, even when the host total is
	// unreadable (the daemon's rule: an unreadable total is not fatal).
	got, note, err = depsMemoryLimit("2GiB", 4, total(0, errors.New("no /proc")))
	if err != nil || got != 2*gib || !strings.Contains(note, "2.0 GiB") {
		t.Errorf(`"2GiB" = %d %q %v`, got, note, err)
	}
	// The escape hatch.
	got, note, err = depsMemoryLimit("none", 4, total(8*gib, nil))
	if err != nil || got != 0 || !strings.Contains(note, "none") {
		t.Errorf(`"none" = %d %q %v`, got, note, err)
	}
	// Garbage refuses, naming the value, before anything runs.
	if _, _, err := depsMemoryLimit("lots", 4, total(8*gib, nil)); err == nil || !strings.Contains(err.Error(), "lots") {
		t.Errorf(`"lots" must be refused naming the value, got %v`, err)
	}
	// An unreadable host with no explicit limit runs unbounded, and says so.
	got, note, err = depsMemoryLimit("", 4, total(0, errors.New("no /proc")))
	if err != nil || got != 0 || !strings.Contains(note, "unbounded") {
		t.Errorf("unreadable host = %d %q %v, want unbounded and said", got, note, err)
	}
}
