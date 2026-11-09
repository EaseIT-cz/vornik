package cli

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/runtime"
)

// GitHub #460 (08-runtime-manager-podman.md S6, "the bench grader and the
// scraper login sidecar get a bound"): both containers ran with no memory
// bound. They now carry --memory=N --memory-swap=N, N the agents' one-slot
// derivation.

func memoryFlags460(args []string) (mem, swap string) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--memory="); ok {
			mem = v
		}
		if v, ok := strings.CutPrefix(a, "--memory-swap="); ok {
			swap = v
		}
	}
	return mem, swap
}

func TestPodmanGrader_BoundsTheContainer_460(t *testing.T) {
	f := &fakeGradeRunner{testOut: "ok  \tscratch/x\t0.01s"}
	g := newTestGrader(t, f)
	g.memoryLimit = 3 << 30
	g.Grade(context.Background(), acceptanceSpec(t))
	var podman []string
	for _, c := range f.calls {
		if c[0] == "podman" {
			podman = c
		}
	}
	mem, swap := memoryFlags460(podman)
	if mem != "3221225472" || swap != mem {
		t.Fatalf("grader argv --memory=%q --memory-swap=%q, want both 3221225472 (no swap); argv %q", mem, swap, podman)
	}
}

func TestScraperLoginArgs_BoundsTheContainer_460(t *testing.T) {
	args := scraperLoginArgs("localhost/scraper-login:x", 2<<30)
	mem, swap := memoryFlags460(args)
	if mem != "2147483648" || swap != mem {
		t.Fatalf("login sidecar argv --memory=%q --memory-swap=%q, want both 2147483648; argv %q", mem, swap, args)
	}
	if args[len(args)-1] != "localhost/scraper-login:x" {
		t.Fatalf("the image must stay the last argument: %q", args)
	}
	if mem, _ := memoryFlags460(scraperLoginArgs("img", 0)); mem != "" {
		t.Fatalf("an unknown host total leaves the sidecar unbounded, got --memory=%s", mem)
	}
}

// On a Linux host the one-slot bound is half of MemTotal, never 0.
func TestOneSlotMemoryLimit_460(t *testing.T) {
	total, err := runtime.HostTotalMemory()
	if err != nil {
		t.Skip("no /proc/meminfo on this host")
	}
	got := oneSlotMemoryLimit()
	if got <= 0 || got > total {
		t.Fatalf("oneSlotMemoryLimit = %d with MemTotal %d, want a bound within the host", got, total)
	}
}
