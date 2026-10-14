package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D5 residual 2 (GitHub #161; onboarding-hardening-design.md "D5 residual 2
// closed", 2026-10-12). The first start after an image alias change found
// the pinned image absent: `podman image inspect` failed, the attempt got no
// --user, and `podman run` pulled the image and ran it as its baked uid, which
// cannot read the contract on a host whose uid differs (the 2026-10-02
// failure). The runtime now pulls the pinned image first and reads the label.

// labelSequence answers each call with the next (id, agnostic, err).
type labelSequence struct {
	calls   int
	answers []labelAnswer
}

type labelAnswer struct {
	id       string
	agnostic bool
	err      error
}

func (l *labelSequence) fn(context.Context, string) (string, bool, error) {
	a := l.answers[min(l.calls, len(l.answers)-1)]
	l.calls++
	return a.id, a.agnostic, a.err
}

type pullSpy struct {
	images []string
	err    error
}

func (p *pullSpy) fn(_ context.Context, image string) error {
	p.images = append(p.images, image)
	return p.err
}

// audit 2026-10-12 (#161): inspect says not-local, the pull succeeds, the
// re-inspect reads the label, and the attempt carries --user once.
func TestSynthesisedUser_PullsAnAbsentImageThenSynthesises(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "keep-id"
	labels := &labelSequence{answers: []labelAnswer{
		{err: fmt.Errorf("podman image inspect %s: %w", testAgentImage, errImageNotLocal)},
		{id: "sha256:aaa", agnostic: true},
	}}
	pulls := &pullSpy{}
	m.imageLabelFunc = labels.fn
	m.imagePullFunc = pulls.fn

	startOnce(t, m)

	if len(pulls.images) != 1 || pulls.images[0] != testAgentImage {
		t.Fatalf("pulls = %v, want exactly one of %s", pulls.images, testAgentImage)
	}
	if labels.calls != 2 {
		t.Fatalf("label reads = %d, want 2 (before and after the pull)", labels.calls)
	}
	if got := userFlags(readAttempts(t, logPath)[0]); len(got) != 1 || got[0] != "1001:1001" {
		t.Fatalf("--user = %v, want [1001:1001] once", got)
	}
}

// audit 2026-10-12 (#161): a failed pull falls back as before — no --user,
// the start still happens.
func TestSynthesisedUser_FailedPullFallsBack(t *testing.T) {
	path, logPath := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "keep-id"
	labels := &labelSequence{answers: []labelAnswer{{err: errImageNotLocal}}}
	pulls := &pullSpy{err: errors.New("registry unreachable")}
	m.imageLabelFunc = labels.fn
	m.imagePullFunc = pulls.fn

	startOnce(t, m)

	if len(pulls.images) != 1 {
		t.Fatalf("pulls = %v, want one attempt", pulls.images)
	}
	if labels.calls != 1 {
		t.Fatalf("label reads = %d, want 1 (no re-read after a failed pull)", labels.calls)
	}
	if got := userFlags(readAttempts(t, logPath)[0]); len(got) != 0 {
		t.Fatalf("--user = %v, want none after a failed pull", got)
	}
}

// audit 2026-10-12 (#161): an inspect failure that is not "image not known"
// does not pull — a pull would fail the same way.
func TestSynthesisedUser_OtherInspectErrorDoesNotPull(t *testing.T) {
	path, _ := recordingPodman(t, `  echo "cid-1"; exit 0`)
	m := d5Manager(path)
	m.userNSMode = "keep-id"
	pulls := &pullSpy{}
	m.imageLabelFunc = (&labelSequence{answers: []labelAnswer{{err: errors.New("podman: cannot connect")}}}).fn
	m.imagePullFunc = pulls.fn

	startOnce(t, m)

	if len(pulls.images) != 0 {
		t.Fatalf("pulls = %v, want none", pulls.images)
	}
}

// audit 2026-10-12 (#161): the real inspect classifies podman's "image not
// known" answer as errImageNotLocal, and a different failure as not.
func TestReadImageUIDAgnostic_ImageNotKnownIsNotLocal(t *testing.T) {
	notKnown := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: `+testAgentImage+`: image not known" >&2
exit 125
`)}
	if _, _, err := notKnown.readImageUIDAgnostic(context.Background(), testAgentImage); !errors.Is(err, errImageNotLocal) {
		t.Fatalf("want errImageNotLocal, got %v", err)
	}
	broken := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: cannot connect to Podman socket" >&2
exit 125
`)}
	if _, _, err := broken.readImageUIDAgnostic(context.Background(), testAgentImage); err == nil || errors.Is(err, errImageNotLocal) {
		t.Fatalf("want a non-not-local error, got %v", err)
	}
}

// audit 2026-10-12 (#161): the default pull runs `podman pull --quiet
// <pinned image>` through the spawn law.
func TestPullAgentImage_RunsAQuietPullOfTheImage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "pull.log")
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
printf '%s ' "$@" >> "`+logPath+`"
exit 0
`)}
	if err := m.pullAgentImage(context.Background(), testAgentImage); err != nil {
		t.Fatalf("pullAgentImage: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "pull --quiet "+testAgentImage {
		t.Fatalf("argv = %q", got)
	}
}
