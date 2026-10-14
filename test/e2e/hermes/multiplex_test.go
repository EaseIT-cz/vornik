package hermes

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The multiplexed-profile arm's verdict (lane design, "Amendment 2026-10-14 —
// the multiplexed-profile arm (O2)"; design 24 O2, hermes-agent#137179 rule
// 11). These pin the decision the arm makes from the daemon's audit rows,
// without podman.

var muxKeys = []laneKey{
	{"default", "broker", "kA", "broker-mail"}, {"default", "memory", "kAm", "assistant-memory"},
	{"beta", "broker", "kB", "broker-b"}, {"beta", "memory", "kBm", "memory-b"},
}

func row(id, key, project, tool string) auditRow { return auditRow{id, key, project, tool} }

// cleanSnapshots is 0.21.5's shape: gamma makes no call, beta uses B's keys,
// default A's.
func cleanSnapshots() multiplexSnapshots {
	s1 := []auditRow{}
	s2 := append(append([]auditRow{}, s1...), row("1", "kB", "broker-b", "catalog"), row("2", "kBm", "memory-b", "recall"))
	s3 := append(append([]auditRow{}, s2...), row("3", "kA", "broker-mail", "catalog"), row("4", "kAm", "assistant-memory", "recall"))
	return multiplexSnapshots{S0: nil, S1: s1, S2: s2, S3: s3}
}

func TestMultiplexVerdict_OwnKeysPass(t *testing.T) {
	problems, denom := multiplexVerdict(muxKeys, cleanSnapshots())
	if len(problems) != 0 {
		t.Fatalf("clean run failed: %v", problems)
	}
	for _, want := range []string{"examined: 4", "gamma window 0", "beta/broker=1", "default/memory=1"} {
		if !strings.Contains(denom, want) {
			t.Errorf("denominator %q lacks %q", denom, want)
		}
	}
}

// The 0.21.4 shape: every profile used the launch (default) profile's keys,
// read from os.environ at register().
func TestMultiplexVerdict_LeakOf0214Fails(t *testing.T) {
	s1 := []auditRow{row("1", "kA", "broker-mail", "catalog")}
	s2 := append(append([]auditRow{}, s1...), row("2", "kA", "broker-mail", "catalog"), row("3", "kAm", "assistant-memory", "recall"))
	s3 := append(append([]auditRow{}, s2...), row("4", "kA", "broker-mail", "catalog"), row("5", "kAm", "assistant-memory", "recall"))
	problems, _ := multiplexVerdict(muxKeys, multiplexSnapshots{S1: s1, S2: s2, S3: s3})
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"gamma (no settings) reached the daemon", "before default's turn, a call carried default's key",
		"beta's broker key never reached", "beta's memory key never reached"} {
		if !strings.Contains(joined, want) {
			t.Errorf("leak verdict lacks %q:\n%s", want, joined)
		}
	}
}

func TestMultiplexVerdict_NoCallsFails(t *testing.T) {
	problems, denom := multiplexVerdict(muxKeys, multiplexSnapshots{})
	if len(problems) != 4 {
		t.Fatalf("an empty scan must fail for each of the four keys, got %v", problems)
	}
	if !strings.Contains(denom, "examined: 0") {
		t.Errorf("denominator %q", denom)
	}
}

func TestMultiplexVerdict_UnknownKeyAndWrongProjectFail(t *testing.T) {
	s := cleanSnapshots()
	s.S3 = append(s.S3, row("9", "kZ", "broker-mail", "list"), row("10", "kB", "broker-mail", "list"))
	problems, _ := multiplexVerdict(muxKeys, s)
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "a key no profile holds") || !strings.Contains(joined, "the key's project is broker-b") {
		t.Fatalf("got %v", problems)
	}
}

// A beta call that lands after S2 (asynchronous work) is no leak, but it does
// not count as beta's own turn either.
func TestMultiplexVerdict_LateBetaRowIsNotALeak(t *testing.T) {
	s := cleanSnapshots()
	s.S3 = append(s.S3, row("11", "kBm", "memory-b", "remember"))
	if problems, _ := multiplexVerdict(muxKeys, s); len(problems) != 0 {
		t.Fatalf("got %v", problems)
	}
}

func TestMultiplexVerdict_DuplicateKeyIDsFail(t *testing.T) {
	keys := append(append([]laneKey{}, muxKeys...), laneKey{"beta", "broker", "kA", "broker-b"})
	problems, _ := multiplexVerdict(keys, cleanSnapshots())
	if !strings.Contains(strings.Join(problems, "\n"), "share the id kA") {
		t.Fatalf("got %v", problems)
	}
}

// Review 849e F4: the rows are split on the unit separator psql is told to
// use (auditFieldSep), so a "|" in a project id or tool name is data.
func TestParseAuditRows(t *testing.T) {
	line := strings.Join([]string{"ta_1", "companion:k1", "broker|b", "mcp__plugin_vornik-companion_vornik__catalog"}, auditFieldSep)
	rows, err := parseAuditRows(line + "\n\n")
	if err != nil || len(rows) != 1 || rows[0] != (auditRow{"ta_1", "k1", "broker|b", "catalog"}) {
		t.Fatalf("rows %v err %v", rows, err)
	}
	if _, err := parseAuditRows(strings.Join([]string{"ta_2", "task-1", "p", "x"}, auditFieldSep)); err == nil {
		t.Fatal("a non-companion row must not parse")
	}
	if _, err := parseAuditRows("ta_3|companion:k1|p|x"); err == nil {
		t.Fatal("a row not split by the unit separator must not parse")
	}
}

// Review 849e F1: the verdict attributes rows to a turn by the snapshot
// taken after it, so it must also refuse snapshots that are not
// cumulative (a row present at one boundary and gone at a later one would
// make newRows mis-attribute everything after it).
func TestMultiplexVerdict_NonCumulativeSnapshotsFail(t *testing.T) {
	s := cleanSnapshots()
	s.S3 = s.S3[2:] // beta's rows vanished between S2 and S3
	problems, _ := multiplexVerdict(muxKeys, s)
	if !strings.Contains(strings.Join(problems, "\n"), "not cumulative") {
		t.Fatalf("got %v", problems)
	}
}

// fakeClock drives settleAuditRows without sleeping.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// Review 849e F1: after a turn the arm snapshots only once the row count
// has held still for the quiet window, so a call still landing is counted
// in its own turn's window.
func TestSettleAuditRows_WaitsForAStableCount(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	reads := 0
	read := func() ([]auditRow, error) {
		reads++
		n := reads
		if n > 3 {
			n = 3 // the third row lands on the third read, then nothing more
		}
		out := make([]auditRow, n)
		for i := range out {
			out[i] = row(string(rune('a'+i)), "kA", "broker-mail", "catalog")
		}
		return out, nil
	}
	rows, err := settleAuditRows(read, time.Second, 10*time.Second, clk.Now, clk.Sleep)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows %d err %v", len(rows), err)
	}
	if reads < 4 {
		t.Fatalf("settled after %d reads, before the count had held for the quiet window", reads)
	}
}

func TestSettleAuditRows_FailsLoudWhenNeverStable(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	n := 0
	read := func() ([]auditRow, error) {
		n++
		out := make([]auditRow, n)
		for i := range out {
			out[i] = row(strings.Repeat("x", i+1), "kA", "broker-mail", "catalog")
		}
		return out, nil
	}
	if _, err := settleAuditRows(read, time.Second, 5*time.Second, clk.Now, clk.Sleep); err == nil || !strings.Contains(err.Error(), "did not settle") {
		t.Fatalf("err %v", err)
	}
}

func TestSettleAuditRows_PropagatesReadErrors(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	boom := errors.New("psql gone")
	if _, err := settleAuditRows(func() ([]auditRow, error) { return nil, boom }, time.Second, 5*time.Second, clk.Now, clk.Sleep); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

func TestVornikToolsOffered(t *testing.T) {
	got := vornikToolsOffered([]string{"memory", "vornik_catalog", "terminal", "vornik_recall"})
	if strings.Join(got, ",") != "vornik_catalog,vornik_recall" {
		t.Fatalf("got %v", got)
	}
}

// Bring-up 2026-10-14 (review 849e F1 follow-up): the gateway's boot warm-up
// runs the launch profile's broker check after /health answers; the arm waits
// for Hermes's own end-of-warm-up line before its first snapshot.
func TestGatewayWarmUpEnded(t *testing.T) {
	for _, c := range []struct {
		logs         string
		done, failed bool
	}{
		{"", false, false},
		{"2026-10-14 10:39:39,130 WARNING gateway.run: Host gateway lock could not be opened", false, false},
		{"2026-10-14 10:39:45,400 INFO gateway.run: Turn machinery warmed in 6.2s (24 tool schema(s) materialized)", true, false},
		{"2026-10-14 10:39:45,400 WARNING gateway.run: Turn-machinery warm-up failed; first inbound turn will initialize lazily", true, true},
	} {
		if done, failed := gatewayWarmUpEnded(c.logs); done != c.done || failed != c.failed {
			t.Errorf("%q: done=%v failed=%v, want %v %v", c.logs, done, failed, c.done, c.failed)
		}
	}
}

// Review add7 F3: a configured key seen only after its own window is named
// as late, distinct from never used, and still fails.
func TestMultiplexVerdict_LateOnlyKeyIsNamedLate(t *testing.T) {
	s := cleanSnapshots()
	// beta's memory row moves from beta's window to default's.
	s.S2 = []auditRow{s.S2[0]}
	s.S3 = append(append([]auditRow{}, s.S2...), row("2", "kBm", "memory-b", "recall"),
		row("3", "kA", "broker-mail", "catalog"), row("4", "kAm", "assistant-memory", "recall"))
	problems, _ := multiplexVerdict(muxKeys, s)
	if len(problems) != 1 || !strings.Contains(problems[0], "beta's memory key reached the daemon only after its turn's window") {
		t.Fatalf("got %v", problems)
	}
}
