package hermes

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	vruntime "vornik.io/vornik/internal/runtime"
)

// Incident 2026-10-14 (found at: production): `make test-e2e-hermes` on the
// shared reference host started llama.cpp with no memory limit while
// production and the bench arm ran; swap filled and the OOM killer took the
// production vornik-scraper container with the lane's llama. These tests pin
// the guardrails the lane design's "Memory guardrails" amendment adds: the
// preflight decision and the per-container limits.

func meminfoText(totalGiB, availGiB, swapTotalGiB, swapFreeGiB float64) string {
	kb := func(g float64) int64 { return int64(g * 1024 * 1024) }
	return fmt.Sprintf("MemTotal:       %d kB\nMemFree:         1000 kB\nMemAvailable:   %d kB\nBuffers:             304 kB\nSwapTotal:      %d kB\nSwapFree:       %d kB\n",
		kb(totalGiB), kb(availGiB), kb(swapTotalGiB), kb(swapFreeGiB))
}

func mustHostMemory(t *testing.T, text string) hostMemory {
	t.Helper()
	h, err := readHostMemory(strings.NewReader(text))
	if err != nil {
		t.Fatalf("readHostMemory: %v", err)
	}
	return h
}

// gateStage is what the lane charges before the gate model starts.
var gateStage = llamaStageItems(laneModels[defaultLaneModel])

func TestReadHostMemory_ParsesTheFourFields(t *testing.T) {
	h := mustHostMemory(t, meminfoText(30, 17, 15, 9))
	if h.Total != 30*gib || h.Available != 17*gib || h.SwapTotal != 15*gib || h.SwapFree != 9*gib {
		t.Fatalf("parsed %+v", h)
	}
	if _, err := readHostMemory(strings.NewReader("MemTotal: 1000 kB\n")); err == nil {
		t.Fatal("a meminfo without MemAvailable must be an error, not zero")
	}
}

func TestMemoryPreflight_EnoughProceedsSilently(t *testing.T) {
	h := mustHostMemory(t, meminfoText(64, 40, 8, 8))
	v := decideMemory("before llama", h, nil, gateStage, false, nil, nil)
	if !v.OK || v.Overridden || v.Message != "" {
		t.Fatalf("enough memory must proceed silently: %+v", v)
	}
}

// The incident's host: 30.6 GiB total, 17.3 GiB available with production
// and the bench arm up. The gate model must not start.
func TestMemoryPreflight_TooLittleFailsNamingNeedAndAvailable(t *testing.T) {
	h := mustHostMemory(t, meminfoText(30.6, 17.3, 15.3, 9.4))
	v := decideMemory("before llama", h, nil, gateStage, false, nil, nil)
	if v.OK {
		t.Fatalf("17.3 GiB available must not admit a 16 GiB model plus Hermes, agents and margin: %+v", v)
	}
	for _, want := range []string{"before llama", "llama.cpp (gpt-oss-20b) 16.28 GiB", "Hermes container 1.00 GiB", "Postgres 0.50 GiB", "daemon (budget) 0.50 GiB", "2 agent slots 2.00 GiB", "MemAvailable 17.3 GiB",
		"margin 3.06 GiB", "needs 23.34 GiB", "SwapFree 9.4 of 15.3 GiB", "bench arm", "VORNIK_E2E_HERMES_MODEL=stub", dedicatedHostEnv + "=1"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, v.Message)
		}
	}
}

func TestMemoryPreflight_ExhaustedSwapFails(t *testing.T) {
	h := mustHostMemory(t, meminfoText(64, 40, 15, 1))
	v := decideMemory("before Postgres", h, nil, stackStageItems(), false, nil, nil)
	if v.OK || !strings.Contains(v.Message, "swap") {
		t.Fatalf("a host with swap nearly exhausted must fail: %+v", v)
	}
	// No swap at all is not "exhausted".
	h = mustHostMemory(t, meminfoText(64, 40, 0, 0))
	if v := decideMemory("before Postgres", h, nil, stackStageItems(), false, nil, nil); !v.OK {
		t.Fatalf("a host without swap must not fail on swap: %+v", v)
	}
}

func TestMemoryPreflight_OverrideProceedsWithAWarning(t *testing.T) {
	h := mustHostMemory(t, meminfoText(30.6, 17.3, 15.3, 9.4))
	v := decideMemory("before llama", h, nil, gateStage, true, nil, nil)
	if !v.OK || !v.Overridden {
		t.Fatalf("the override must proceed: %+v", v)
	}
	if !strings.HasPrefix(v.Message, "WARNING") || !strings.Contains(v.Message, dedicatedHostEnv+"=1") || !strings.Contains(v.Message, "needs 23.34 GiB") {
		t.Fatalf("an overridden failure must still be printed in full, naming the override:\n%s", v.Message)
	}
	// The override changes nothing when the check passes.
	h = mustHostMemory(t, meminfoText(64, 40, 8, 8))
	if v := decideMemory("before llama", h, nil, gateStage, true, nil, nil); !v.OK || v.Overridden || v.Message != "" {
		t.Fatalf("a passing check with the override set must be silent: %+v", v)
	}
}

func TestMemoryPreflight_UnreadableFails(t *testing.T) {
	v := decideMemory("before Postgres", hostMemory{}, errors.New("open /proc/meminfo: no such file"), stackStageItems(), false, nil, nil)
	if v.OK || !strings.Contains(v.Message, "/proc/meminfo") {
		t.Fatalf("an unreadable meminfo is not proof of safety: %+v", v)
	}
}

func TestMemoryMargin(t *testing.T) {
	if got := memoryMargin(8 * gib); got != 2*gib {
		t.Errorf("small host margin = %d, want the 2 GiB floor", got)
	}
	if got := memoryMargin(40 * gib); got != 4*gib {
		t.Errorf("40 GiB host margin = %d, want 10%%", got)
	}
}

func TestPodmanRunArgs_LimitAndNoSwap(t *testing.T) {
	got := podmanRunArgs(memHermes, "--rm", "img", "cmd")
	// The lane labels ride the builder (review 1787 N1).
	want := append(strings.Fields("run --memory=1073741824 --memory-swap=1073741824 --oom-score-adj=1000"), laneLabels()...)
	want = append(want, "--rm", "img", "cmd")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("podmanRunArgs = %v, want %v", got, want)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a non-positive limit must panic: it would run the container unbounded")
		}
	}()
	unbounded := int64(0) // a variable: the census scan rejects a literal 0
	podmanRunArgs(unbounded, "img")
}

func TestLaneModelLimitsCoverTheFile(t *testing.T) {
	if len(laneModels) == 0 {
		t.Fatal("no lane models examined")
	}
	for id, m := range laneModels {
		if m.FileBytes <= 0 || m.OverheadBytes <= 0 {
			t.Errorf("%s: FileBytes %d and OverheadBytes %d must both be set", id, m.FileBytes, m.OverheadBytes)
		}
		if m.memLimit() != m.FileBytes+m.OverheadBytes {
			t.Errorf("%s: limit %d is not file + overhead", id, m.memLimit())
		}
	}
	if _, ok := laneModels[defaultLaneModel]; !ok {
		t.Fatalf("the gate model %q has no entry", defaultLaneModel)
	}
	for _, l := range []int64{memPostgres, memHermes, memAgent, budgetDaemon} {
		if l <= 0 {
			t.Fatalf("a lane limit is unset: %d", l)
		}
	}
	// The daemon enforces the agent limit from the config string; the
	// preflight charges memAgent. They must be the same number.
	if got, err := vruntime.ParseMemoryLimit(laneAgentMemoryLimit); err != nil || got != memAgent {
		t.Fatalf("laneAgentMemoryLimit %q parses to %d (%v), want memAgent %d", laneAgentMemoryLimit, got, err, memAgent)
	}
}

// Review dc82 F6: the override is for a dedicated host; a host running
// anything else (production, the bench arm) is refused it.
func TestMemoryPreflight_OverrideRefusedWithForeignContainers(t *testing.T) {
	h := mustHostMemory(t, meminfoText(30.6, 17.3, 15.3, 9.4))
	v := decideMemory("before llama", h, nil, gateStage, true, []string{"vornik-scraper"}, nil)
	if v.OK || !strings.Contains(v.Message, "vornik-scraper") || !strings.Contains(v.Message, "not a dedicated host") {
		t.Fatalf("the override must be refused beside foreign containers: %+v", v)
	}
	cs := []podmanContainer{
		{ID: "a", State: "running", Names: []string{"vornik-scraper"}},
		{ID: "b", State: "running", Names: []string{"vornik-e2e-pg-x"}, Labels: map[string]string{laneLabelKey: laneLabelVal}},
		{ID: "c", State: "exited", Names: []string{"old"}},
	}
	// An unlistable host is not proven dedicated (review 1787 N2).
	v = decideMemory("before llama", h, nil, gateStage, true, nil, errors.New("exit status 125"))
	if v.OK || !strings.Contains(v.Message, "could not be listed") {
		t.Fatalf("the override must be refused when podman ps cannot be read: %+v", v)
	}
	if got := foreignContainers(cs, nil); len(got) != 1 || got[0] != "vornik-scraper" {
		t.Fatalf("foreignContainers = %v, want only the running unlabelled one", got)
	}
}

// Review dc82 F2: the llama stage is the lane's peak, so it charges every
// ceiling co-resident with llama, including the running Postgres and daemon.
func TestLlamaStageChargesThePeak(t *testing.T) {
	var got int64
	names := map[string]bool{}
	for _, it := range llamaStageItems(laneModels[defaultLaneModel]) {
		got += it.Bytes
		names[it.What] = true
	}
	want := memPostgres + budgetDaemon + laneAgentSlots*memAgent + laneModels[defaultLaneModel].memLimit() + memHermes
	if got != want || !names["Postgres"] || !names["daemon (budget)"] {
		t.Fatalf("llama stage charges %d (%v), want %d with Postgres and the daemon", got, names, want)
	}
}

// Each Hermes-image container re-checks only itself: the earlier stages
// reserved the lane's own growth (lane design, "Memory guardrails").
func TestHermesStageChargesOnlyTheContainer(t *testing.T) {
	items := hermesStageItems()
	if len(items) != 1 || items[0].Bytes != memHermes {
		t.Fatalf("hermes stage = %v, want only the Hermes container's %d", items, memHermes)
	}
}

// Review 11c0 F8: the override vouches that a dedicated host may be filled;
// it does not stand in for a measurement that never happened.
func TestMemoryPreflight_OverrideDoesNotLiftAnUnreadableMeminfo(t *testing.T) {
	v := decideMemory("before llama", hostMemory{}, errors.New("open /proc/meminfo: permission denied"), gateStage, true, nil, nil)
	if v.OK || v.Overridden || !strings.Contains(v.Message, "/proc/meminfo") {
		t.Fatalf("an unreadable meminfo must fail even with the override: %+v", v)
	}
}
