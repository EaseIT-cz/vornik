package hermes

// Memory guardrails (lane design, amendment 2026-10-14). Incident
// 2026-10-14: the lane's llama.cpp ran with no memory limit on the shared
// reference host; swap filled and the OOM killer took a production container
// with it. Every container the lane starts itself (podmanRunArgs) now
// carries a limit with no swap, and each heavy stage first checks that the
// host has the memory for it. Agent containers the DAEMON starts for the
// lane carry the lane config's 1 GiB limit through --memory only, so podman
// still allows them swap up to that limit (corrected 2026-10-14; lane design,
// the correction below the memory-limits table).

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	vruntime "vornik.io/vornik/internal/runtime"
)

const (
	mib int64 = 1 << 20
	gib int64 = 1 << 30
)

// Per-container limits. The basis of each is in the lane design's table.
const (
	memPostgres int64 = 512 * mib // measured 6.7 MB idle; the lane DB is tiny
	memHermes   int64 = 1 * gib   // measured 109 MiB startup peak
	memAgent    int64 = 1 * gib   // daemon-spawned agent containers (--memory only, swap allowed); measured 12.3 MiB peak
	// budgetDaemon is charged, not enforced: the daemon is a host process.
	budgetDaemon int64 = 512 * mib
	// laneAgentSlots is the lane config's scheduler.max_concurrent_tasks.
	laneAgentSlots = 2
	// laneAgentMemoryLimit is written to the lane config's
	// runtime.agent_memory_limit; it must equal memAgent.
	laneAgentMemoryLimit = "1GiB"

	minMemoryMargin int64 = 2 * gib
	// dedicatedHostEnv=1 lets a failing preflight proceed, with a warning.
	dedicatedHostEnv = "VORNIK_E2E_DEDICATED_HOST"
)

// podmanRunArgs is the only place a podman run argv starts (the census test
// TestEveryLaneContainerCarriesAMemoryLimit enforces it). --memory-swap equal
// to --memory gives the container no swap, so it is OOM-killed inside its own
// cgroup instead of pushing the host into swap; --oom-score-adj=1000 makes it
// the global OOM killer's first choice should the host run out anyway.
func podmanRunArgs(limit int64, args ...string) []string {
	if limit <= 0 {
		panic(fmt.Sprintf("hermes e2e: container memory limit %d would run it unbounded", limit))
	}
	n := strconv.FormatInt(limit, 10)
	// The lane labels are added here too, so every lane container carries
	// them: the reaper and the dedicated-host override both key on them
	// (review 1787 N1).
	out := append([]string{"run", "--memory=" + n, "--memory-swap=" + n, "--oom-score-adj=1000"}, laneLabels()...)
	return append(out, args...)
}

// memItem is one thing a stage is about to add to the host.
type memItem struct {
	What  string
	Bytes int64
}

// hostMemory is the part of /proc/meminfo the preflight reads, in bytes.
type hostMemory struct {
	Total, Available, SwapTotal, SwapFree int64
}

func readHostMemory(r io.Reader) (hostMemory, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return hostMemory{}, err
	}
	var h hostMemory
	for _, f := range []struct {
		name string
		dst  *int64
	}{{"MemTotal", &h.Total}, {"MemAvailable", &h.Available}, {"SwapTotal", &h.SwapTotal}, {"SwapFree", &h.SwapFree}} {
		v, err := vruntime.ParseMeminfoField(bytes.NewReader(raw), f.name)
		if err != nil {
			return hostMemory{}, err
		}
		*f.dst = v
	}
	return h, nil
}

// memoryMargin is what the preflight leaves for the rest of the host:
// max(2 GiB, 10% of MemTotal). A policy number (lane design).
func memoryMargin(total int64) int64 {
	if m := total / 10; m > minMemoryMargin {
		return m
	}
	return minMemoryMargin
}

// memVerdict is the preflight's decision. Message is empty on a clean pass.
type memVerdict struct {
	OK         bool
	Overridden bool
	Message    string
}

func gibStr(b int64, prec int) string {
	return strconv.FormatFloat(float64(b)/float64(gib), 'f', prec, 64) + " GiB"
}

func agentSlotsItem() memItem {
	return memItem{fmt.Sprintf("%d agent slots", laneAgentSlots), laneAgentSlots * memAgent}
}

// stackStageItems is charged before Postgres starts: Postgres, the daemon
// (budgeted, not measured on the lane and not enforced) and the agent
// containers the daemon will start, which have no stage of their own.
func stackStageItems() []memItem {
	return []memItem{{"Postgres", memPostgres}, {"daemon (budget)", budgetDaemon}, agentSlotsItem()}
}

// llamaStageItems is the lane's peak, charged before llama.cpp starts: the
// ceiling of everything co-resident with it. Postgres and the daemon are
// charged again although they are running, because they may grow to their
// ceilings after this point (review dc82 F2); their current use, tens of MB,
// is thereby counted twice, which errs safe.
func llamaStageItems(m laneModel) []memItem {
	return append(stackStageItems(), memItem{"llama.cpp (" + m.Name + ")", m.memLimit()}, memItem{"Hermes container", memHermes})
}

// hermesStageItems is charged before each Hermes-image container: only that
// container. It catches a host that changed since the earlier stages; the
// lane's own growth was reserved by them.
func hermesStageItems() []memItem {
	return []memItem{{"Hermes container", memHermes}}
}

// decideMemory is the preflight decision: fail when the host cannot hold what
// the stage adds plus the margin, when swap is nearly exhausted, or when
// meminfo could not be read. override turns a failure into a warning, unless
// foreign (running containers that are not this lane's) is non-empty or could
// not be listed (foreignErr): a host running anything else, or one not proven
// to run nothing else, is not dedicated (reviews dc82 F6, 1787 N2).
func decideMemory(stage string, h hostMemory, readErr error, items []memItem, override bool, foreign []string, foreignErr error) memVerdict {
	var problem string
	if readErr != nil {
		// A hard failure the override does not lift: it vouches that the
		// host may be filled, not for a measurement that never happened
		// (review 11c0 F8).
		return memVerdict{Message: fmt.Sprintf("memory preflight (%s): cannot read /proc/meminfo, so the host is not proven to have room: %v", stage, readErr)}
	}
	{
		var need int64
		parts := make([]string, 0, len(items)+1)
		for _, it := range items {
			need += it.Bytes
			parts = append(parts, it.What+" "+gibStr(it.Bytes, 2))
		}
		margin := memoryMargin(h.Total)
		need += margin
		parts = append(parts, "margin "+gibStr(margin, 2))
		have := fmt.Sprintf("MemAvailable %s, SwapFree %s of %s", gibStr(h.Available, 1), strings.TrimSuffix(gibStr(h.SwapFree, 1), " GiB"), gibStr(h.SwapTotal, 1))
		switch {
		case h.Available < need:
			problem = fmt.Sprintf("memory preflight (%s): needs %s available (%s) but the host has %s.",
				stage, gibStr(need, 2), strings.Join(parts, " + "), have)
		case h.SwapTotal > 0 && h.SwapFree < h.SwapTotal/10:
			problem = fmt.Sprintf("memory preflight (%s): swap is nearly exhausted (%s): the host is already under memory pressure.", stage, have)
		}
	}
	if problem == "" {
		return memVerdict{OK: true}
	}
	advice := " Stop what is using memory first (on the shared reference host: the bench arm), run with VORNIK_E2E_HERMES_MODEL=stub (never the release gate), or, on a dedicated host only, set " + dedicatedHostEnv + "=1."
	if override && foreignErr != nil {
		return memVerdict{Message: problem + " " + dedicatedHostEnv + "=1 is refused: the running containers could not be listed (" +
			foreignErr.Error() + "), so the host is not proven dedicated." + advice}
	}
	if override && len(foreign) > 0 {
		return memVerdict{Message: problem + " " + dedicatedHostEnv + "=1 is refused: this host runs containers that are not this lane's (" +
			strings.Join(foreign, ", ") + "), so it is not a dedicated host." + advice}
	}
	if override {
		return memVerdict{OK: true, Overridden: true, Message: "WARNING: " + dedicatedHostEnv + "=1 overrides a failing check: " + problem}
	}
	return memVerdict{Message: problem + advice}
}
