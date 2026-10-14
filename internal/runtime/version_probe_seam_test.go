package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// audit 2026-10-12: internal/service tests reached the real `podman version`
// availability probe through NewContainer -> runtime.New, and one was killed at
// its fixed 5 s bound under parallel `make test` load (#127). WithVersionProbe
// replaces the exec; these pin that the seam is the only thing that runs, that
// its result is judged by podmanVersionUsable exactly as the exec's is, and
// that the daemon's 5 s bound still reaches it (runtime LLD, amendment
// 2026-10-12).

const probeDoc = `{"Client":{"APIVersion":"5.0.0","Version":"5.0.0"}}`

// fakePodman writes an executable named podman that records every call; the
// seam must leave it untouched.
func fakePodman(t *testing.T) (path, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	path = filepath.Join(dir, "podman")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\necho '" + probeDoc + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, marker
}

func TestNew_VersionProbeSeamReplacesTheExec(t *testing.T) {
	path, marker := fakePodman(t)
	calls := 0
	var deadline time.Time
	m, err := New(WithPodmanPath(path), WithVersionProbe(func(ctx context.Context) ([]byte, error) {
		calls++
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("the probe context carries no deadline; the daemon's bound must reach it")
		}
		deadline = d
		return []byte(probeDoc), nil
	}))
	if err != nil {
		t.Fatalf("New with a seam probe: %v", err)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if until := time.Until(deadline); until > 5*time.Second {
		t.Fatalf("probe deadline %s away, want <= the daemon's 5 s", until)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("podman was executed despite the seam (marker: %v)", err)
	}
	if !m.IsAvailable() || calls != 2 {
		t.Fatalf("IsAvailable must use the same seam (calls=%d)", calls)
	}
}

func TestNew_VersionProbeSeamFailureIsPodmanNotAvailable(t *testing.T) {
	_, err := New(WithPodmanPath("/nonexistent/vornik-unit-test/podman"), WithVersionProbe(func(context.Context) ([]byte, error) {
		return []byte("not a version document"), errors.New("exit status 125")
	}))
	var pna *PodmanNotAvailableError
	if !errors.As(err, &pna) {
		t.Fatalf("err = %v, want PodmanNotAvailableError", err)
	}
}

// The vornik-ee#66 forgiveness applies to the seam's result too: answered,
// then died, is available.
func TestNew_VersionProbeSeamAnsweredThenDiedIsAvailable(t *testing.T) {
	_, err := New(WithPodmanPath("/nonexistent/vornik-unit-test/podman"), WithVersionProbe(func(context.Context) ([]byte, error) {
		return []byte(probeDoc), errors.New("signal: killed")
	}))
	if err != nil {
		t.Fatalf("a valid document with an error must be forgiven: %v", err)
	}
}
