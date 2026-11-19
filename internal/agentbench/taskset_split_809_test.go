package agentbench

// GitHub #809 (Amendment 2026-11-18 (#809), change 2): the learning study
// learns on a warm-up part of the polyglot set and is measured on a held-out
// part. scripts/agentbench-split-taskset.py writes both files; this test
// keeps them honest: disjoint, together the whole set, each task unchanged,
// and balanced by language within one task.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadSet809(t *testing.T, path string) []TaskSpec {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []TaskSpec
	if err := json.Unmarshal(raw, &tasks); err != nil {
		t.Fatal(err)
	}
	return tasks
}

func TestTaskSetSplit809_WarmupAndMeasurePartitionThePolyglotSet(t *testing.T) {
	src := loadSet809(t, "tasksets/polyglot-go-python-v1.json")
	warm := loadSet809(t, "tasksets/polyglot-go-python-v1-warmup.json")
	measure := loadSet809(t, "tasksets/polyglot-go-python-v1-measure.json")
	// The parsed task, re-encoded: equal parsed tasks give the same
	// task-set digest (TaskBody reads the same fields plus the seed files,
	// which both parts share by path).
	body := func(ts TaskSpec) string {
		b, err := json.Marshal(ts)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	bySrc := map[string]string{}
	for _, ts := range src {
		bySrc[ts.ID] = body(ts)
	}
	seen := map[string]string{}
	lang := map[string]map[string]int{"warmup": {}, "measure": {}}
	for name, part := range map[string][]TaskSpec{"warmup": warm, "measure": measure} {
		for _, ts := range part {
			if prev, dup := seen[ts.ID]; dup {
				t.Fatalf("%s is in both %s and %s", ts.ID, prev, name)
			}
			seen[ts.ID] = name
			if b, ok := bySrc[ts.ID]; !ok || b != body(ts) {
				t.Fatalf("%s in %s differs from the source set (present: %v)", ts.ID, name, ok)
			}
			lang[name][strings.SplitN(ts.ID, "-", 3)[1]]++
		}
	}
	if len(seen) != len(src) {
		t.Fatalf("the parts hold %d tasks, the set %d", len(seen), len(src))
	}
	for _, l := range []string{"go", "python"} {
		w, m := lang["warmup"][l], lang["measure"][l]
		if w == 0 || m == 0 || float64(w)/float64(w+m) < 0.35 || float64(w)/float64(w+m) > 0.45 {
			t.Fatalf("%s: %d warm-up, %d measured; want about 40%% warm-up in each language", l, w, m)
		}
	}
}
