package hermes

// The multiplexed-profile arm's verdict (lane design, "Amendment 2026-10-14 —
// the multiplexed-profile arm (O2)"). One Hermes gateway serves three
// profiles: default and beta, each with its own Vornik keys on its own
// projects, and gamma with no settings. The arm snapshots the daemon's
// companion audit rows around each profile's turn; this file decides, from
// those snapshots alone, whether every profile reached the daemon with its own
// keys and gamma with none. Untagged, so its unit tests run in make test.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// auditRow is one companion tool_audit_log row: task_id is "companion:<key id>".
type auditRow struct {
	ID, KeyID, Project, Tool string
}

// laneKey is one minted companion key and the profile that holds it.
type laneKey struct {
	Profile, Role, ID, Project string // Role: "broker" or "memory"
}

func (k laneKey) label() string { return k.Profile + "/" + k.Role }

// multiplexSnapshots are the audit rows at the four boundaries: before gamma's
// turn (S0), after gamma (S1), after beta (S2) and at the end (S3). Turns run
// gamma, beta, default, so gamma's window precedes every other call.
type multiplexSnapshots struct {
	S0, S1, S2, S3 []auditRow
}

// newRows returns the rows of after that are not in before.
func newRows(before, after []auditRow) []auditRow {
	seen := make(map[string]bool, len(before))
	for _, r := range before {
		seen[r.ID] = true
	}
	var out []auditRow
	for _, r := range after {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// multiplexVerdict lists every problem (empty: pass) and a denominator line
// naming how many rows were examined per window and per key, so a pass is
// never an empty scan.
func multiplexVerdict(keys []laneKey, s multiplexSnapshots) (problems []string, denominator string) {
	byID := map[string]laneKey{}
	for _, k := range keys {
		if prev, dup := byID[k.ID]; dup {
			// Two profiles' keys sharing an id would attribute one's calls
			// to the other (review da4c F9).
			problems = append(problems, fmt.Sprintf("keys %s and %s share the id %s", prev.label(), k.label(), k.ID))
		}
		byID[k.ID] = k
	}
	describe := func(r auditRow) string {
		if k, ok := byID[r.KeyID]; ok {
			return fmt.Sprintf("%s (%s key %s) on %s", r.Tool, k.label(), r.KeyID, r.Project)
		}
		return fmt.Sprintf("%s (unknown key %s) on %s", r.Tool, r.KeyID, r.Project)
	}
	// The windows below are set differences of consecutive snapshots, which
	// attribute a row to a turn only if every snapshot holds all the rows of
	// the one before it (review 849e F1). Check it rather than assume it.
	bounds := []struct {
		name string
		rows []auditRow
	}{{"S0", s.S0}, {"S1", s.S1}, {"S2", s.S2}, {"S3", s.S3}}
	for i := 1; i < len(bounds); i++ {
		if lost := newRows(bounds[i].rows, bounds[i-1].rows); len(lost) > 0 {
			problems = append(problems, fmt.Sprintf("audit snapshots are not cumulative: %d row(s) of %s are missing from %s",
				len(lost), bounds[i-1].name, bounds[i].name))
		}
	}
	gamma := newRows(s.S0, s.S1)
	beta := newRows(s.S1, s.S2)
	def := newRows(s.S2, s.S3)
	all := newRows(s.S0, s.S3)

	// gamma has no settings: any call in its window is another profile's key
	// (nothing else had run yet). The 0.21.4 leak.
	for _, r := range gamma {
		problems = append(problems, "gamma (no settings) reached the daemon: "+describe(r))
	}
	// Every row: a known key, on that key's own project.
	perKey := map[string]int{}
	for _, r := range all {
		k, ok := byID[r.KeyID]
		if !ok {
			problems = append(problems, "a call with a key no profile holds: "+describe(r))
			continue
		}
		perKey[k.label()]++
		if r.Project != k.Project {
			problems = append(problems, fmt.Sprintf("%s: the key's project is %s", describe(r), k.Project))
		}
	}
	// Up to S2 only gamma and beta have had a turn: a default key there is a leak.
	for _, r := range append(append([]auditRow(nil), gamma...), beta...) {
		if k, ok := byID[r.KeyID]; ok && k.Profile == "default" {
			problems = append(problems, "before default's turn, a call carried default's key: "+describe(r))
		}
	}
	// Each configured profile's own keys were used in or after its own window.
	// A key seen only after its window is reported as late, not as never
	// used (review add7 F3): the settled boundaries make that unlikely, and
	// it still fails, because the window is what ties the call to the turn.
	need := func(window []auditRow, profile, role string) {
		has := func(rows []auditRow) bool {
			for _, r := range rows {
				if k, ok := byID[r.KeyID]; ok && k.Profile == profile && k.Role == role {
					return true
				}
			}
			return false
		}
		switch {
		case has(window):
		case has(all):
			problems = append(problems, fmt.Sprintf("%s's %s key reached the daemon only after its turn's window (late, not a leak)", profile, role))
		default:
			problems = append(problems, fmt.Sprintf("%s's %s key never reached the daemon in its turn", profile, role))
		}
	}
	need(beta, "beta", "broker")
	need(beta, "beta", "memory")
	need(def, "default", "broker")
	need(def, "default", "memory")

	labels := make([]string, 0, len(keys))
	for _, k := range keys {
		labels = append(labels, fmt.Sprintf("%s=%d", k.label(), perKey[k.label()]))
	}
	sort.Strings(labels)
	denominator = fmt.Sprintf("companion audit rows examined: %d (gamma window %d, beta window %d, default window %d; %d before gamma's turn, not examined); per key: %s",
		len(all), len(gamma), len(beta), len(def), len(s.S0), strings.Join(labels, ", "))
	return problems, denominator
}

// auditFieldSep is the field separator the arm passes to psql (-F): the
// ASCII unit separator, which no key id, project id or tool name holds, so a
// "|" in one is data and not a field boundary (review 849e F4).
const auditFieldSep = "\x1f"

// settleAuditRows reads the audit rows until their count has held still for
// quiet, polling at a quarter of it, and fails when that has not happened
// within timeout. The arm snapshots a window's end only once settled, so a
// call still landing after the turn's HTTP answer is attributed to its own
// turn, and an ordering assumption that stops holding fails loudly here
// instead of mis-attributing rows (review 849e F1). now and sleep are seams
// for the unit tests. A stable count means stable rows only because
// tool_audit_log is append-only (one INSERT per companion tools/call,
// recordCompanionToolAudit); a row rewritten in place would not be seen here,
// though one that vanishes between boundaries fails the verdict's
// cumulative check (review add7 F1).
func settleAuditRows(read func() ([]auditRow, error), quiet, timeout time.Duration,
	now func() time.Time, sleep func(time.Duration)) ([]auditRow, error) {
	start := now()
	rows, err := read()
	if err != nil {
		return nil, err
	}
	changed := start
	for now().Sub(changed) < quiet {
		if now().Sub(start) >= timeout {
			return nil, fmt.Errorf("companion audit rows did not settle: still changing after %s (last count %d)", timeout, len(rows))
		}
		sleep(quiet / 4)
		next, err := read()
		if err != nil {
			return nil, err
		}
		if len(next) != len(rows) {
			changed = now()
		}
		rows = next
	}
	return rows, nil
}

// parseAuditRows reads psql -tA -F auditFieldSep output:
// id, task_id, project_id, tool_name.
func parseAuditRows(out string) ([]auditRow, error) {
	var rows []auditRow
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, auditFieldSep)
		if len(f) != 4 || !strings.HasPrefix(f[1], "companion:") {
			return nil, fmt.Errorf("unexpected audit row %q", line)
		}
		rows = append(rows, auditRow{ID: f[0], KeyID: strings.TrimPrefix(f[1], "companion:"), Project: f[2],
			Tool: strings.TrimPrefix(f[3], "mcp__plugin_vornik-companion_vornik__")})
	}
	return rows, nil
}

// gatewayWarmUpEnded reads the Hermes gateway's launch-home logs and reports
// whether its boot turn-machinery warm-up has ended (done), and whether it
// ended by failing. The lines are Hermes's own (gateway/run_startup.py
// _warm_turn_prerequisites at the pin): "Turn machinery warmed in ..." on
// success, "Turn-machinery warm-up failed" otherwise. Hermes logs the line
// after the warm-up's probe has returned, so after the daemon has written the
// probe's audit row (the INSERT precedes the answer). Should a later Hermes
// log it earlier, that row lands in gamma's window and the arm fails red
// naming a default-key row there; it cannot turn a leak green (review add7 F2).
func gatewayWarmUpEnded(logs string) (done, failed bool) {
	if strings.Contains(logs, "Turn-machinery warm-up failed") {
		return true, true
	}
	return strings.Contains(logs, "Turn machinery warmed in "), false
}

// toolLister is a profile's model endpoint in the arm: a scripted stub or a
// recorder in front of llama.cpp. Watch/WatchSeen let the arm check that each
// profile's turn reached that profile's own model endpoint (the positive
// control, review da4c F2). Declared here, untagged, with the assertions
// below, so make test fails to compile if either type stops providing what
// the arm relies on (review 849e F3).
type toolLister interface {
	ToolsSeen() []string
	Watch(string)
	WatchSeen() bool
}

var (
	_ toolLister = (*LLMStub)(nil)
	_ toolLister = (*ToolRecorder)(nil)
)

// vornikToolsOffered returns the Vornik tools among a profile's offered tools.
func vornikToolsOffered(tools []string) []string {
	var out []string
	for _, n := range tools {
		if strings.HasPrefix(n, "vornik_") {
			out = append(out, n)
		}
	}
	return out
}
