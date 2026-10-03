package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// D5 (onboarding-hardening-design.md, 2026-10-02). The 2026.10.3 deploy on the
// reference host (uid 1001, userns_mode keep-id, run_as_user "") pulled the
// published uid-1000 image; with no --user the agent ran as 1000 and every step
// died "contract mount unusable: cannot read /app/input/task.json (running as
// uid:gid 1000:1000)". The fix: on a rootless keep-id attempt with run_as_user
// empty and a uid-agnostic image, pass --user <daemon uid>:<gid>, once, before
// the image, on that attempt only (R3, R8).

const testAgentImage = "localhost/vornik-agent:test"

// recordingPodman is a fake podman that appends each `run` argv (one arg per
// line, attempts separated by "----") to a log, answers `run` per the rule
// given, and fails every other verb.
func recordingPodman(t *testing.T, runBody string) (path, logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "runs.log")
	path = writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  printf '%s\n' "$@" >> "`+logPath+`"
  echo "----" >> "`+logPath+`"
`+runBody+`
fi
echo "unexpected podman verb: $*" >&2
exit 1
`)
	return path, logPath
}

func readAttempts(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "----" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// userFlags returns the values of every --user in argv.
func userFlags(argv []string) []string {
	var users []string
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--user" {
			users = append(users, argv[i+1])
		}
	}
	return users
}

func indexOf(argv []string, s string) int {
	for i, a := range argv {
		if a == s {
			return i
		}
	}
	return -1
}

type labelCall struct{ calls int }

func (l *labelCall) fn(id string, agnostic bool, err error) func(context.Context, string) (string, bool, error) {
	return func(context.Context, string) (string, bool, error) {
		l.calls++
		return id, agnostic, err
	}
}

func d5Manager(podmanPath string) *Manager {
	return &Manager{
		podmanPath: podmanPath,
		rootless:   true,
		daemonUID:  1001,
		daemonGID:  1001,
		logger:     zerolog.Nop(),
	}
}

func startOnce(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.StartContainer(context.Background(), &ContainerConfig{
		Image: testAgentImage, ProjectID: "proj", Role: "coder", TaskID: "task-d5",
	}); err != nil {
		t.Fatalf("StartContainer() error = %v", err)
	}
}

// The label row: --user <daemon uid>:<gid> exactly once, before the image.
func TestStartContainer_D5_LabelRowSynthesisesUserOnceBeforeImage(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "keep-id"
	labels := &labelCall{}
	m.imageLabelFunc = labels.fn("sha256:aaa", true, nil)

	startOnce(t, m)

	attempts := readAttempts(t, logPath)
	if len(attempts) != 1 {
		t.Fatalf("want 1 podman run, got %d", len(attempts))
	}
	argv := attempts[0]
	if got := userFlags(argv); len(got) != 1 || got[0] != "1001:1001" {
		t.Fatalf("--user values = %v, want exactly [1001:1001]; argv=%v", got, argv)
	}
	img := indexOf(argv, testAgentImage)
	if img != len(argv)-1 {
		t.Fatalf("image is not the last arg: %v", argv)
	}
	if u := indexOf(argv, "--user"); u > img {
		t.Fatalf("--user after the image: %v", argv)
	}
	if labels.calls != 1 {
		t.Fatalf("label read %d times, want 1", labels.calls)
	}
}

// The other R3 rows never synthesise; rows that cannot reach the label row do
// not read the label at all.
func TestStartContainer_D5_OtherRowsDoNotSynthesise(t *testing.T) {
	cases := []struct {
		name        string
		runAsUser   string
		userns      string
		rootless    bool
		agnostic    bool
		wantUsers   []string
		wantLabelRd int
	}{
		{"configured run_as_user", "1000:1000", "keep-id", true, true, []string{"1000:1000"}, 0},
		{"unlabelled keep-id", "", "keep-id", true, false, nil, 1},
		{"configured host userns", "", "host", true, true, nil, 0},
		{"configured private userns", "", "private", true, true, nil, 0},
		{"rootful keep-id", "", "keep-id", false, true, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
			m := d5Manager(path)
			m.userNSMode = tc.userns
			m.runAsUser = tc.runAsUser
			m.rootless = tc.rootless
			labels := &labelCall{}
			m.imageLabelFunc = labels.fn("sha256:aaa", tc.agnostic, nil)

			startOnce(t, m)

			argv := readAttempts(t, logPath)[0]
			got := userFlags(argv)
			if strings.Join(got, ",") != strings.Join(tc.wantUsers, ",") {
				t.Fatalf("--user values = %v, want %v; argv=%v", got, tc.wantUsers, argv)
			}
			if labels.calls != tc.wantLabelRd {
				t.Fatalf("label read %d times, want %d", labels.calls, tc.wantLabelRd)
			}
		})
	}
}

// R1 corrected / R8: with userns_mode unset, the default attempt carries no
// synthesised --user, and the keep-id attempt the chain reaches after a
// namespace-setup failure carries it exactly once.
func TestStartContainer_D5_FallbackChainSynthesisesOnKeepIDAttemptOnly(t *testing.T) {
	path, logPath := recordingPodman(t, `  for arg in "$@"; do
    if [[ "$arg" == "keep-id" ]]; then echo "cid-keep"; exit 0; fi
  done
  echo 'Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1'
  exit 125`)
	m := d5Manager(path)
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)

	startOnce(t, m)

	attempts := readAttempts(t, logPath)
	if len(attempts) != 2 {
		t.Fatalf("want 2 attempts (default, keep-id), got %d: %v", len(attempts), attempts)
	}
	if got := userFlags(attempts[0]); len(got) != 0 {
		t.Fatalf("default attempt carries --user %v; argv=%v", got, attempts[0])
	}
	if indexOf(attempts[0], "--userns") != -1 {
		t.Fatalf("default attempt carries --userns: %v", attempts[0])
	}
	keep := attempts[1]
	if got := userFlags(keep); len(got) != 1 || got[0] != "1001:1001" {
		t.Fatalf("keep-id attempt --user = %v, want exactly [1001:1001]; argv=%v", got, keep)
	}
	if u, img := indexOf(keep, "--user"), indexOf(keep, testAgentImage); u > img || img != len(keep)-1 {
		t.Fatalf("keep-id attempt order wrong: %v", keep)
	}
}

// A label that cannot be read is treated as absent: today's behaviour, no
// --user, and the start still happens.
func TestStartContainer_D5_UnreadableLabelFallsBackToNoUser(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "keep-id"
	m.imageLabelFunc = (&labelCall{}).fn("", false, errors.New("image not known"))

	startOnce(t, m)

	if got := userFlags(readAttempts(t, logPath)[0]); len(got) != 0 {
		t.Fatalf("--user = %v, want none when the label is unreadable", got)
	}
}

// R7: one log line per image id when the label path synthesises --user,
// naming image, daemon uid, attempt and reason. A new id (re-pull) logs again.
func TestStartContainer_D5_LogsOncePerImageID(t *testing.T) {
	path, _ := recordingPodman(t, `  echo "cid-1"; exit 0`)
	var buf bytes.Buffer
	m := d5Manager(path)
	m.logger = zerolog.New(&buf)
	m.userNSMode = "keep-id"
	id := "sha256:aaa"
	m.imageLabelFunc = func(context.Context, string) (string, bool, error) { return id, true, nil }

	startOnce(t, m)
	startOnce(t, m)
	if n := strings.Count(buf.String(), synthesisedUserLogMsg); n != 1 {
		t.Fatalf("synthesised-user log lines = %d after two starts of one image id, want 1\n%s", n, buf.String())
	}
	line := ""
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, synthesisedUserLogMsg) {
			line = l
		}
	}
	for _, want := range []string{`"image":"` + testAgentImage + `"`, `"image_id":"sha256:aaa"`, `"daemon_uid":1001`, `"attempt":"keep-id"`, `"reason":"label"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %s: %s", want, line)
		}
	}

	id = "sha256:bbb"
	startOnce(t, m)
	if n := strings.Count(buf.String(), synthesisedUserLogMsg); n != 2 {
		t.Fatalf("synthesised-user log lines = %d after a new image id, want 2", n)
	}
}

// The default label reader: one `podman image inspect` of the pinned image,
// presence-based (R5), null labels are no labels.
func TestReadImageUIDAgnostic_ParsesInspect(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantID   string
		wantAgn  bool
		wantFail bool
	}{
		{"labelled", `sha256:aaa	{"io.vornik.agent.uid-agnostic":"1","x":"y"}`, "sha256:aaa", true, false},
		{"labelled empty value", `sha256:aaa	{"io.vornik.agent.uid-agnostic":""}`, "sha256:aaa", true, false},
		{"unlabelled", `sha256:bbb	{"org.opencontainers.image.version":"2026.9.4"}`, "sha256:bbb", false, false},
		{"null labels", `sha256:ccc	null`, "sha256:ccc", false, false},
		{"garbage", `not an inspect`, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "image" && "$2" == "inspect" ]]; then
  printf '%s\n' '`+tc.out+`'
  exit 0
fi
exit 1
`)}
			id, agn, err := m.readImageUIDAgnostic(context.Background(), testAgentImage)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("want error, got id=%q agnostic=%v", id, agn)
				}
				return
			}
			if err != nil || id != tc.wantID || agn != tc.wantAgn {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, nil)", id, agn, err, tc.wantID, tc.wantAgn)
			}
		})
	}
}

// New() records the daemon's own identity, which the resolver needs.
func TestNew_RecordsDaemonIdentity(t *testing.T) {
	m, err := New(WithPodmanPath(writeFakePodman(t, `#!/usr/bin/env bash
echo '{"Client":{"Version":"5.0.0"}}'
`)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if m.daemonUID != os.Getuid() || m.daemonGID != os.Getgid() || m.rootless != (os.Geteuid() != 0) {
		t.Fatalf("identity = (%d, %d, rootless=%v), want (%d, %d, %v)",
			m.daemonUID, m.daemonGID, m.rootless, os.Getuid(), os.Getgid(), os.Geteuid() != 0)
	}
}
