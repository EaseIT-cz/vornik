//go:build e2e_hermes

package hermes

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Hermes approval transport design §6 (E2E): with the Vornik transport
// selected in a real Hermes (the pinned image, the plugin from this tree,
// vornikctl agent connect run inside the image), a command Hermes's own
// safety rules flag files a host_action request through vornikctl agent
// host-approval. Each arm is asserted on Hermes's side, by a marker file the
// command leaves only if Hermes ran it, and on the row:
//   - approve: the phone answers "once"; the marker exists and the row is
//     approved by the lane's device with choice once;
//   - deny: the phone answers "deny"; no marker, the row is rejected;
//   - expire: nobody answers; Hermes's timeout denies, no marker, and the
//     expiry tick closes the row as expired.
//
// Why a driver and not `hermes -z`: -z is single-query mode, which sets
// HERMES_YOLO_MODE=1 and HERMES_SINGLE_QUERY_SESSION=1, so no approval is
// ever asked there (hermes_cli/oneshot.py; tools/approval.py
// _unattended_contexts). The driver calls Hermes's own terminal tool in an
// interactive CLI context (HERMES_INTERACTIVE=1), so Hermes's detection,
// gate, transport invocation, decision validation and command execution are
// Hermes's own code; only the model turn that would choose the command is
// skipped.

const hostDriver = `import json, os, sys
os.environ["HERMES_INTERACTIVE"] = "1"
os.environ.pop("HERMES_YOLO_MODE", None)
from tools.terminal_tool import terminal_tool
print(json.dumps({"result": terminal_tool(command=sys.argv[1])}))
`

// hostApprovalTimeout is Hermes's approvals.timeout for the arm: short, so
// the expire arm ends in under two ticks.
const hostApprovalTimeout = 20

type hostRow struct {
	id, status, choice, device string
}

func hostActionRow(t *testing.T, db *sql.DB, marker string) (hostRow, error) {
	t.Helper()
	var r hostRow
	err := db.QueryRowContext(context.Background(), `SELECT id, status, COALESCE(decided_choice,''), COALESCE(decided_by_device,'')
		FROM agent_approval_requests WHERE kind = 'host_action' AND rendered::text LIKE '%' || $1 || '%'`, marker).
		Scan(&r.id, &r.status, &r.choice, &r.device)
	return r, err
}

// runHostDriver runs the driver in the Hermes image as the Hermes user, the
// way hermesAgentRun runs hermes -z.
func runHostDriver(t *testing.T, s *stack, ctl, command string) (string, error) {
	uid, gid := hermesUser(t)
	run(t, "podman", "unshare", "chown", "-R", uid+":"+gid, filepath.Join(s.hermesHome, ".config"))
	cmd := exec.Command("podman", "run", "--rm", "--network", "host",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config",
		hermesImage, "python3", "/opt/data/host-driver.py", command)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	return out.String(), err
}

func TestA5_HermesHostApprovals(t *testing.T) {
	s := startStack(t)
	ensureHermesImage(t)
	s.hermesModelName, s.llamaURL = scriptedModel, "http://127.0.0.1:9/v1" // no model turn is taken
	prepareHermesHome(t, s)
	cfgPath := filepath.Join(s.hermesHome, "config.yaml")
	raw, _ := os.ReadFile(cfgPath)
	// manual: no guardian-LLM step before the human is asked.
	writeFile(t, cfgPath, string(raw)+"approvals:\n  mode: manual\n  timeout: 20\n")
	writeFile(t, filepath.Join(s.hermesHome, "host-driver.py"), hostDriver)
	phone := pairPhone(t, s)
	ctl := buildStaticCtl(t, s)

	connect := exec.Command("podman", "run", "--rm", "--network", "host",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config", "-e", "VORNIK_API_KEY="+s.adminKey,
		hermesImage, "/usr/local/bin/vornikctl", "agent", "connect", "hermes", "--url", s.apiURL)
	out, err := connect.CombinedOutput()
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if err != nil {
		t.Fatalf("connect inside the Hermes image: %v\n%s", err, out)
	}

	// Control: before the transport is selected, Hermes does not ask Vornik.
	// With no one at Hermes's own prompt (stdin is closed) it never runs the
	// command, and no request is filed.
	db := laneDB(t, s)
	if res, _ := runHostDriver(t, s, ctl, "rm -rf /opt/data/scratch-c && touch /opt/data/marker-control"); fileExists(filepath.Join(s.hermesHome, "marker-control")) {
		t.Fatalf("precondition: Hermes ran a flagged command with nobody to answer:\n%s", tail(res, 2000))
	}
	if _, err := hostActionRow(t, db, "marker-control"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("precondition: a request was filed with the transport unselected: %v", err)
	}

	// hermes vornik approvals on, inside the image, through Hermes's own
	// config writer.
	uid, gid := hermesUser(t)
	run(t, "podman", "unshare", "chown", "-R", uid+":"+gid, s.hermesHome)
	on := exec.Command("podman", "run", "--rm", "--network", "host",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config", hermesImage, "hermes", "vornik", "approvals", "on")
	onOut, onErr := on.CombinedOutput()
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if b, _ := os.ReadFile(cfgPath); onErr != nil || !strings.Contains(string(b), "transport: vornik") || strings.Contains(string(b), "transport_fallback") {
		t.Fatalf("hermes vornik approvals on: %v\n%s\nconfig:\n%s", onErr, tail(string(onOut), 2000), b)
	}

	var laneDevice string
	if err := db.QueryRowContext(context.Background(), `SELECT id FROM approver_devices WHERE revoked_at IS NULL`).Scan(&laneDevice); err != nil {
		t.Fatal(err)
	}

	// arm runs one flagged command while the phone does answer (or does
	// nothing, for ""), and returns what Hermes printed.
	arm := func(name, answer string) string {
		t.Helper()
		marker := "marker-" + name
		command := "rm -rf /opt/data/scratch-" + name + " && touch /opt/data/" + marker
		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		go func() {
			o, e := runHostDriver(t, s, ctl, command)
			done <- result{o, e}
		}()
		if answer != "" {
			deadline := time.Now().Add(slow(time.Duration(hostApprovalTimeout-4) * time.Second))
			answered := false
			for !answered && time.Now().Before(deadline) {
				pending, err := phone.Pending()
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pending {
					if strings.Contains(p.Sentence, "Hermes (hermes) wants to run a command its safety rules flagged") {
						if err := phone.Answer(p.ID, answer); err != nil {
							t.Fatalf("%s: answering %s: %v", name, answer, err)
						}
						answered = true
					}
				}
				time.Sleep(300 * time.Millisecond)
			}
			if !answered {
				r := <-done
				t.Fatalf("%s: no host action reached the phone; Hermes printed:\n%s", name, tail(r.out, 3000))
			}
		}
		r := <-done
		t.Logf("%s: Hermes printed:\n%s", name, tail(r.out, 1500))
		return r.out
	}

	// Approve: the marker exists, and the row says which device allowed it.
	arm("approve", "once")
	if !fileExists(filepath.Join(s.hermesHome, "marker-approve")) {
		t.Fatal("approve: the phone allowed the command, but Hermes did not run it")
	}
	if r, err := hostActionRow(t, db, "marker-approve"); err != nil || r.status != "approved" || r.choice != "once" || r.device != laneDevice {
		t.Fatalf("approve: row %+v %v, want approved once by %s", r, err, laneDevice)
	}

	// Deny: no marker.
	arm("deny", "deny")
	if fileExists(filepath.Join(s.hermesHome, "marker-deny")) {
		t.Fatal("deny: Hermes ran a command the phone denied")
	}
	if r, err := hostActionRow(t, db, "marker-deny"); err != nil || r.status != "rejected" || r.choice != "deny" || r.device != laneDevice {
		t.Fatalf("deny: row %+v %v", r, err)
	}

	// Expire: nobody answers. The CLI gives up 2 s before Hermes does, both
	// deny, and the once-a-minute tick closes the row.
	arm("expire", "")
	if fileExists(filepath.Join(s.hermesHome, "marker-expire")) {
		t.Fatal("expire: Hermes ran a command nobody answered")
	}
	waitFor(t, "the expiry tick to close the unanswered host action", slow(150*time.Second), func() bool {
		r, err := hostActionRow(t, db, "marker-expire")
		return err == nil && r.status == "expired"
	})
	if r, _ := hostActionRow(t, db, "marker-expire"); r.device != "" || r.choice != "" {
		t.Fatalf("expire: the row records a decision: %+v", r)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
