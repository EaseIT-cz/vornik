package runtime

import (
	"context"
	"strings"
	"testing"
)

// GitHub #663 (onboarding-hardening design D6). Found at: e2e-lane (a
// hand-configured rootless bench instance): with userns_mode unset, the
// default namespace started the container, the agent (uid 1000 in the image)
// could not read its bind mounts ("contract mount unusable"), and the
// start-time fallback never fired. Regression test at: integration, the real
// StartContainer against a recording podman. Lower because: the e2e lane
// needs a rootless host and an agent image.

// Rootless and unset: keep-id first, never the default namespace, with the
// D5 identity on the keep-id attempt.
func TestStartContainer_663_RootlessUnsetStartsWithKeepID(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-keep"; exit 0`)
	m := d5Manager(path)
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)

	startOnce(t, m)

	attempts := readAttempts(t, logPath)
	if len(attempts) != 1 {
		t.Fatalf("want one attempt, got %d: %v", len(attempts), attempts)
	}
	if i := indexOf(attempts[0], "--userns"); i == -1 || attempts[0][i+1] != "keep-id" {
		t.Fatalf("the first rootless attempt must be --userns keep-id: %v", attempts[0])
	}
	if got := userFlags(attempts[0]); len(got) != 1 || got[0] != "1001:1001" {
		t.Fatalf("keep-id attempt --user = %v, want [1001:1001]", got)
	}
}

// Rootless, keep-id fails at namespace setup, host allowed: the chain moves to
// host and never tries the default namespace.
func TestStartContainer_663_RootlessFallsBackToHostNotDefault(t *testing.T) {
	path, logPath := recordingPodman(t, `  for arg in "$@"; do
    if [[ "$arg" == "host" ]]; then echo "cid-host"; exit 0; fi
  done
  echo 'Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1'
  exit 125`)
	m := d5Manager(path)
	m.allowHostUserns = true
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)

	startOnce(t, m)

	attempts := readAttempts(t, logPath)
	if len(attempts) != 2 {
		t.Fatalf("want 2 attempts (keep-id, host), got %d: %v", len(attempts), attempts)
	}
	for i, want := range []string{"keep-id", "host"} {
		j := indexOf(attempts[i], "--userns")
		if j == -1 || attempts[i][j+1] != want {
			t.Fatalf("attempt %d: want --userns %s: %v", i, want, attempts[i])
		}
	}
}

// An explicit userns_mode still gets exactly one attempt.
func TestStartContainer_663_ExplicitModeIsOneAttempt(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "host"
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)

	startOnce(t, m)

	attempts := readAttempts(t, logPath)
	if len(attempts) != 1 {
		t.Fatalf("want one attempt, got %d", len(attempts))
	}
	if j := indexOf(attempts[0], "--userns"); j == -1 || attempts[0][j+1] != "host" {
		t.Fatalf("explicit host: %v", attempts[0])
	}
}

// A configured run_as_user wins on the rootless keep-id attempt.
func TestStartContainer_663_RunAsUserWinsOnRootlessKeepID(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-keep"; exit 0`)
	m := d5Manager(path)
	m.runAsUser = "2000:2000"
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)

	startOnce(t, m)

	a := readAttempts(t, logPath)[0]
	if got := userFlags(a); len(got) != 1 || got[0] != "2000:2000" {
		t.Fatalf("--user = %v, want the configured [2000:2000]", got)
	}
}

// Rootless, keep-id fails at namespace setup, host not allowed: one attempt,
// and the error names keep-id's prerequisites (the earlier failure point D6
// accepts).
func TestStartContainer_663_RootlessKeepIDFailureNamesPrerequisites(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo 'Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1'
  exit 125`)
	m := d5Manager(path)
	m.imageLabelFunc = (&labelCall{}).fn("sha256:aaa", true, nil)
	_, err := m.StartContainer(context.Background(), &ContainerConfig{
		Image: testAgentImage, ProjectID: "proj", Role: "coder", TaskID: "task-d6",
	})
	if err == nil {
		t.Fatal("a keep-id setup failure with host disallowed must fail the start")
	}
	if n := len(readAttempts(t, logPath)); n != 1 {
		t.Fatalf("want one attempt (keep-id), got %d", n)
	}
	if !strings.Contains(err.Error(), "/etc/subuid") {
		t.Fatalf("the error must name keep-id's prerequisites: %v", err)
	}
}
