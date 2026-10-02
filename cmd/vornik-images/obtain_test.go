package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

// The obtain step (design §S2.2/§S2.6). Every seam is injected, so the pull
// paths are deterministic rather than dependent on a registry being up.

const (
	digestA  = "sha256:aaaa000000000000000000000000000000000000000000000000000000000000"
	eeHead   = "1111111111111111111111111111111111111111"
	ceCommit = "2222222222222222222222222222222222222222"
)

var agent = imagemanifest.Image{
	Tag:           imagemanifest.AgentImageTag,
	Containerfile: "images/vornik-agent/Containerfile",
	Context:       ".",
	Condition:     imagemanifest.ConditionAlways,
}

var broker = imagemanifest.Image{
	Tag:           "localhost/vornik-broker:latest",
	Containerfile: "images/vornik-broker/Containerfile",
	Context:       ".",
	Condition:     imagemanifest.ConditionAlways,
}

func baseOpts(t *testing.T) obtainOpts {
	t.Helper()
	t.Setenv(imagemanifest.ObtainedPathEnv, filepath.Join(t.TempDir(), "obtained.json"))
	return obtainOpts{
		arch:       "amd64",
		head:       ceCommit,
		recordPath: filepath.Join(t.TempDir(), "no-such-record.json"),
		inspect:    func(string) (imagemanifest.LocalImage, error) { return imagemanifest.LocalImage{}, nil },
		resolve:    func(string) (string, error) { return "", imagemanifest.ErrReferenceAbsent },
		pull:       func(string) error { return nil },
		tag:        func(string, string) error { return nil },
		remove:     func(string) error { return nil },
		log:        func(string, ...any) {},
	}
}

// A successful pull means NO build is emitted for that image.
func TestSuccessfulPullEmitsNoBuild(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return digestA, nil }
	var pulled string
	o.pull = func(ref string) error { pulled = ref; return nil }

	build, err := runObtain([]imagemanifest.Image{agent}, o)
	if err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	if len(build) != 0 {
		t.Errorf("got %d rows to build, want 0 — a pulled image must not also be built", len(build))
	}
	want := "ghcr.io/easeit-cz/vornik-agent@" + digestA
	if pulled != want {
		t.Errorf("pulled %q, want %q — every obtain pulls BY DIGEST (§S2.2)", pulled, want)
	}

	rec, err := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	if err != nil {
		t.Fatalf("LoadObtained: %v", err)
	}
	got, ok := rec.MethodFor(agent.Tag)
	if !ok || got.Method != imagemanifest.MethodPulled {
		t.Errorf("provenance = %+v, want method %q (contract C5)", got, imagemanifest.MethodPulled)
	}
	if got.ResolvedFrom != imagemanifest.CommitTag(agent.Tag, ceCommit) {
		t.Errorf("ResolvedFrom = %q, want the tag we resolved through", got.ResolvedFrom)
	}
}

// A failed pull cleans the partial image, falls back to a build, and the run
// still succeeds.
func TestFailedPullCleansAndFallsBackToBuild(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return digestA, nil }
	o.pull = func(string) error { return errors.New("layer 3/9: unexpected EOF") }
	var removed string
	o.remove = func(ref string) error { removed = ref; return nil }

	build, err := runObtain([]imagemanifest.Image{agent}, o)
	if err != nil {
		t.Fatalf("a failed pull must fall back, not abort: %v", err)
	}
	if len(build) != 1 || build[0].Tag != agent.Tag {
		t.Fatalf("got %v, want the agent row queued for a local build", build)
	}
	if removed == "" {
		t.Error("the partial pull was not cleaned — podman can leave a manifest with " +
			"incomplete layers, and building over it is how a broken image reaches a task (§S2.2)")
	}

	rec, _ := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	if got, _ := rec.MethodFor(agent.Tag); got.Method != imagemanifest.MethodBuilt {
		t.Errorf("provenance = %q, want %q — the host built it, and must say so",
			got.Method, imagemanifest.MethodBuilt)
	}
}

// A host already holding the target digest neither pulls nor builds.
func TestCurrentImageDoesNeither(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return digestA, nil }
	o.inspect = func(string) (imagemanifest.LocalImage, error) {
		return imagemanifest.LocalImage{Present: true, Digests: []string{digestA}}, nil
	}
	o.pull = func(string) error { t.Fatal("must not pull an image already at the target digest"); return nil }

	build, err := runObtain([]imagemanifest.Image{agent}, o)
	if err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	if len(build) != 0 {
		t.Errorf("got %d rows to build, want 0", len(build))
	}
}

// THE FINDING-2 REGRESSION. Registry unreachable + an image present: leave it
// alone, and emit NO build row.
func TestUnreachableRegistryEmitsNoBuild(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return "", errors.New("dial tcp: i/o timeout") }
	o.inspect = func(string) (imagemanifest.LocalImage, error) {
		return imagemanifest.LocalImage{Present: true, Digests: []string{digestA}, Revision: ceCommit}, nil
	}
	o.pull = func(string) error { t.Fatal("must not pull when the registry is unreachable"); return nil }
	var logged []string
	o.log = func(f string, _ ...any) { logged = append(logged, f) }

	build, err := runObtain([]imagemanifest.Image{agent}, o)
	if err != nil {
		t.Fatalf("an unreachable registry is a state, not a failure: %v", err)
	}
	if len(build) != 0 {
		t.Fatalf("got %d rows to build, want 0 — rebuilding here churns on every update forever "+
			"and replaces a verifiable pulled image with an unverifiable local one (§S2.3)", len(build))
	}
	if !strings.Contains(strings.Join(logged, "\n"), "LEFT AS IS") {
		t.Errorf("the operator was not told: %v", logged)
	}
}

// Air-gapped FIRST install: nothing to leave alone, so build.
func TestUnreachableRegistryWithNoImageBuilds(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return "", errors.New("no route to host") }

	build, err := runObtain([]imagemanifest.Image{agent}, o)
	if err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	if len(build) != 1 {
		t.Errorf("got %d rows, want 1 — contract C7, the local build path survives Stage 2", len(build))
	}
}

// Host-built images never touch the registry (§S2.7).
func TestHostBuiltImagesNeverResolve(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { t.Fatal("a localhost/ tag must not be resolved"); return "", nil }
	o.inspect = func(string) (imagemanifest.LocalImage, error) {
		return imagemanifest.LocalImage{Present: true, Revision: ceCommit}, nil
	}
	o.head = ceCommit

	build, err := runObtain([]imagemanifest.Image{broker}, o)
	if err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	if len(build) != 0 {
		t.Errorf("got %d rows, want 0 — the broker is current at HEAD", len(build))
	}
}

// MIXED PROVENANCE on one host (round-1 finding 7): a pulled agent and a
// locally built broker, decided by different rules in the same run.
func TestMixedProvenanceOnOneHost(t *testing.T) {
	o := baseOpts(t)
	o.head = ceCommit
	o.resolve = func(string) (string, error) { return digestA, nil }
	o.inspect = func(tag string) (imagemanifest.LocalImage, error) {
		if tag == broker.Tag {
			// Drifted: its revision is not HEAD.
			return imagemanifest.LocalImage{Present: true, Revision: eeHead}, nil
		}
		return imagemanifest.LocalImage{}, nil
	}

	build, err := runObtain([]imagemanifest.Image{agent, broker}, o)
	if err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	if len(build) != 1 || build[0].Tag != broker.Tag {
		t.Fatalf("got %v, want only the broker queued to build (the agent was pulled)", build)
	}

	rec, _ := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	a, _ := rec.MethodFor(agent.Tag)
	b, _ := rec.MethodFor(broker.Tag)
	if a.Method != imagemanifest.MethodPulled || b.Method != imagemanifest.MethodBuilt {
		t.Errorf("provenance: agent=%q broker=%q, want pulled/built — the dual mode is "+
			"intentional and this is where that is asserted", a.Method, b.Method)
	}
}

// A CORRUPT release record must abort, not silently degrade to the commit tag:
// the host cannot know what the release declared, and obtaining something the
// release never named is the failure the record exists to prevent.
func TestCorruptReleaseRecordAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "images.json")
	if err := writeFile(path, `{"version":99}`); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t)
	o.recordPath = path

	if _, err := runObtain([]imagemanifest.Image{agent}, o); err == nil {
		t.Fatal("a corrupt release record was read as success")
	}
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }

// Issue #17 (2026-10-01): a pull by digest creates no tag, and nothing tagged
// the result, so the agent image tag (then ghcr.io/…/vornik-agent:latest) named nothing (a first
// obtain) or the OLD image (an update) after an update that reported success.
func TestPullTagsTheDigestWithTheDeploymentTag(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return digestA, nil }
	var order []string
	o.pull = func(ref string) error { order = append(order, "pull "+ref); return nil }
	o.tag = func(src, dst string) error { order = append(order, "tag "+src+" "+dst); return nil }

	if _, err := runObtain([]imagemanifest.Image{agent}, o); err != nil {
		t.Fatalf("runObtain: %v", err)
	}
	ref := "ghcr.io/easeit-cz/vornik-agent@" + digestA
	want := []string{"pull " + ref, "tag " + ref + " " + agent.Tag}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", order, want)
	}
}

// The tag is what the runtime resolves. Failing to set it is fatal, like
// failing to record the obtain: step 3b runs before the cutover, and going on
// would start a daemon whose agent tag names the wrong image or nothing.
func TestTagFailureIsFatalAndNothingIsRecorded(t *testing.T) {
	o := baseOpts(t)
	o.resolve = func(string) (string, error) { return digestA, nil }
	o.tag = func(string, string) error { return errors.New("tag: no space left on device") }

	if _, err := runObtain([]imagemanifest.Image{agent}, o); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("runObtain err = %v, want the tag failure", err)
	}
	rec, err := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	if err != nil {
		t.Fatalf("LoadObtained: %v", err)
	}
	if got, ok := rec.MethodFor(agent.Tag); ok {
		t.Errorf("recorded %+v for an image whose tag was never set", got)
	}
}

// Issue #15 (2026-10-01): with no skopeo installed the update said the
// registry could not be reached, while GHCR was reachable. The action (leave,
// or build when there is no image) was right; the words sent the operator to
// debug a network that was fine.
func TestMissingResolverIsNamedNotReportedAsAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		want    string
	}{
		{"existing image is left", true, "LEFT AS IS"},
		{"no image is built", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOpts(t)
			o.resolve = func(string) (string, error) { return "", imagemanifest.ErrResolverAbsent }
			o.inspect = func(string) (imagemanifest.LocalImage, error) {
				return imagemanifest.LocalImage{Present: tc.present}, nil
			}
			var logged strings.Builder
			o.log = func(f string, a ...any) { logged.WriteString(fmt.Sprintf(f, a...) + "\n") }

			build, err := runObtain([]imagemanifest.Image{agent}, o)
			if err != nil {
				t.Fatalf("runObtain: %v", err)
			}
			if tc.present != (len(build) == 0) {
				t.Errorf("build = %d rows with present=%v", len(build), tc.present)
			}
			out := logged.String()
			if !strings.Contains(out, "skopeo") {
				t.Errorf("the message does not name skopeo:\n%s", out)
			}
			if strings.Contains(out, "could not be reached") {
				t.Errorf("the message still blames the registry:\n%s", out)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("missing %q:\n%s", tc.want, out)
			}
		})
	}
}

// skopeoResolve with no skopeo on PATH reports ErrResolverAbsent.
func TestSkopeoResolveWithoutSkopeoIsResolverAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := skopeoResolve("ghcr.io/easeit-cz/vornik-agent:sha-123456789012"); !errors.Is(err, imagemanifest.ErrResolverAbsent) {
		t.Fatalf("err = %v, want ErrResolverAbsent", err)
	}
}
