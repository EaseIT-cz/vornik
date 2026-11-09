package gitbackup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fakeRun answers by subcommand and records every call.
type fakeRun struct {
	out   map[string]string
	err   map[string]error
	calls []string
}

func (f *fakeRun) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	key := args[0]
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		key = args[i]
		break
	}
	return []byte(f.out[key]), f.err[key]
}

func (f *fakeRun) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestDirtyTrackedChanges_IgnoredFileAloneWritesNoRef_675(t *testing.T) {
	f := &fakeRun{out: map[string]string{"status": " M BACKLOG.md\n"}}
	DirtyTrackedChanges(context.Background(), f.run, "BACKLOG.md", zerolog.Nop())
	if f.called("stash") || f.called("update-ref") {
		t.Fatalf("only the ignored file is dirty: no stash, no ref; calls %q", f.calls)
	}
}

func TestDirtyTrackedChanges_AnotherChangeKeepsTheWholeTree_675(t *testing.T) {
	f := &fakeRun{out: map[string]string{
		"status": " M BACKLOG.md\nM  src/a.go\n",
		"stash":  "0123456789abcdef0123\n",
	}}
	DirtyTrackedChanges(context.Background(), f.run, "BACKLOG.md", zerolog.Nop())
	if !f.called("update-ref refs/vornik/discarded/") || !f.called("-dirty-0123456789ab 0123456789abcdef0123") {
		t.Fatalf("a change beside the ignored file must be backed up; calls %q", f.calls)
	}
}

func TestDirtyTrackedChanges_NoIgnoreBacksUpAnyChange(t *testing.T) {
	f := &fakeRun{out: map[string]string{"status": " M BACKLOG.md\n", "stash": "abc\n"}}
	DirtyTrackedChanges(context.Background(), f.run, "", zerolog.Nop())
	if !f.called("update-ref refs/vornik/discarded/") {
		t.Fatalf("with no ignore path every tracked change is backed up; calls %q", f.calls)
	}
}

func TestDirtyTrackedChanges_CleanOrFailedStatusWritesNoRef(t *testing.T) {
	for name, f := range map[string]*fakeRun{
		"clean":         {out: map[string]string{"status": ""}},
		"status failed": {err: map[string]error{"status": errors.New("boom")}},
		"stash failed":  {out: map[string]string{"status": " M a\n"}, err: map[string]error{"stash": errors.New("boom")}},
	} {
		DirtyTrackedChanges(context.Background(), f.run, "", zerolog.Nop())
		if f.called("update-ref") {
			t.Errorf("%s: no ref expected; calls %q", name, f.calls)
		}
	}
}

func TestLocalOnlyCommits(t *testing.T) {
	ahead := &fakeRun{out: map[string]string{"rev-list": "2\n", "rev-parse": "abc1234\n"}}
	LocalOnlyCommits(context.Background(), ahead.run, "main", zerolog.Nop())
	if !ahead.called("rev-list --count origin/main..HEAD") || !ahead.called("-abc1234 HEAD") {
		t.Fatalf("two local-only commits must be kept in a ref; calls %q", ahead.calls)
	}
	even := &fakeRun{out: map[string]string{"rev-list": "0\n"}}
	LocalOnlyCommits(context.Background(), even.run, "main", zerolog.Nop())
	if even.called("update-ref") {
		t.Fatalf("no local-only commit: no ref; calls %q", even.calls)
	}
}

func TestPorcelainPath(t *testing.T) {
	for line, want := range map[string]string{
		" M BACKLOG.md":             "BACKLOG.md",
		"M  https://docs.vornik.io": "https://docs.vornik.io",
		"R  old.md -> BACKLOG.md":   "BACKLOG.md",
		"M":                         "",
		// A quoted path never equals a plain ignore path: counted as another change.
		` M "BACK LOG.md"`: `"BACK LOG.md"`,
	} {
		if got := porcelainPath(line); got != want {
			t.Errorf("porcelainPath(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestFailuresNeverPanicOrBlock(t *testing.T) {
	boom := errors.New("boom")
	for _, f := range []*fakeRun{
		{err: map[string]error{"rev-list": boom}},
		{out: map[string]string{"rev-list": "1\n"}, err: map[string]error{"rev-parse": boom}},
		{out: map[string]string{"rev-list": "1\n", "rev-parse": "abc\n"}, err: map[string]error{"update-ref": boom}},
	} {
		LocalOnlyCommits(context.Background(), f.run, "main", zerolog.Nop())
		if f.err["update-ref"] == nil && f.called("update-ref") {
			t.Errorf("a failed lookup must not write a ref; calls %q", f.calls)
		}
	}
	for _, f := range []*fakeRun{
		{out: map[string]string{"status": " M a\n", "stash": "\n"}},
		{out: map[string]string{"status": " M a\n", "stash": "abc\n"}, err: map[string]error{"update-ref": boom}},
	} {
		DirtyTrackedChanges(context.Background(), f.run, "", zerolog.Nop())
		if f.out["stash"] == "\n" && f.called("update-ref") {
			t.Errorf("an empty stash id must not write a ref; calls %q", f.calls)
		}
	}
}
