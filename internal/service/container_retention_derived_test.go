package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/retention"
)

// GitHub #394: the daemon's sweeper tick prunes the upgrade dumps and states
// the denominators of all three derived copies, even when nothing was pruned
// (retention design §8.2.4).
func TestLogDerivedCopySweep_PrunesDumpsAndStatesDenominators(t *testing.T) {
	root := t.TempDir()
	started := time.Now().UTC().Add(-40 * 24 * time.Hour)
	dir := filepath.Join(root, retention.UpgradeBackupPrefix+started.Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "STATE.txt"),
		[]byte(retention.UpgradeCompletedKey+"="+started.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(dir, "vornik-vornik-"+started.Format("20060102")+".dump")
	if err := os.WriteFile(dump, []byte("PGDMP"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	c := &Container{Logger: zerolog.New(&buf)}
	c.logDerivedCopySweep(retention.GlobalCounts{BlackboxTraceCacheExamined: 7, BlackboxTraceCache: 2, ProjectGistsExamined: 3},
		retention.UpgradeDumpPolicy{Root: root})

	if _, err := os.Stat(dump); !os.IsNotExist(err) {
		t.Errorf("a dump 40 days past its upgrade must be pruned by the sweeper tick (stat err %v)", err)
	}
	line := buf.String()
	for _, want := range []string{
		`"message":"retention derived copies"`,
		`"blackbox_trace_cache_examined":7`, `"blackbox_trace_cache_pruned":2`,
		`"project_gists_examined":3`, `"project_gists_pruned":0`,
		`"upgrade_dumps_examined":1`, `"upgrade_dumps_pruned":1`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s:\n%s", want, line)
		}
	}

	// Nothing to prune: the line is still logged, with its zeros.
	buf.Reset()
	c.logDerivedCopySweep(retention.GlobalCounts{}, retention.UpgradeDumpPolicy{Root: t.TempDir()})
	if !strings.Contains(buf.String(), `"upgrade_dumps_examined":0`) || !strings.Contains(buf.String(), `"blackbox_trace_cache_examined":0`) {
		t.Errorf("an idle tick must still state its denominators:\n%s", buf.String())
	}
}
