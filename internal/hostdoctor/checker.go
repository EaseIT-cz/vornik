// Package hostdoctor holds the doctor checks that must run a program on the
// host: podman (runtime, local images, an image's baked uid, and the sandbox
// tool probes), skopeo (published digests) and systemctl (which stacks this
// host intends to run). It is linked
// only by vornikctl, never by a daemon main.
//
// Process-spawn law, S2 (https://docs.vornik.io):
// these checks ran inside POST /api/v1/doctor, so a REST request reached a
// process spawn on the daemon host. The daemon's doctor now reports only what
// it holds, plus a "host_checks" entry pointing here; `vornikctl doctor` runs
// these checks locally and merges them into the same report.
package hostdoctor

import (
	"context"

	"vornik.io/vornik/internal/imagemanifest"
)

// Check is one host check's verdict, in the same JSON shape as the daemon's
// doctor checks so vornikctl can merge the two into one report.
type Check struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"` // OK, WARNING, ERROR, SKIPPED
	Message string   `json:"message"`
	Items   []string `json:"items,omitempty"`
	Fixed   int      `json:"fixed,omitempty"`
}

// Checker runs the host checks. The function fields are seams: nil uses the
// real host implementation, and tests inject fakes so no test shells out.
type Checker struct {
	// configDir is the configs tree (swarms/ names the agent images).
	configDir string

	// usernsMode is the daemon config's runtime.userns_mode ("", "host",
	// "keep-id"). checkAgentImageUID uses it to decide whether the keep-id
	// subuid preflight applies (F3b — second guard for the rootless workspace
	// "Permission denied" incident; see agent_image_uid.go).
	usernsMode string

	// bakedUIDFunc reads the agent image's baked-in uid
	// (`podman run --rm --entrypoint id <image> -u`). Nil ⇒ realBakedUID.
	bakedUIDFunc func(ctx context.Context, image string) (int, error)
	// subuidOKFunc is the keep-id subuid preflight (checks /etc/subuid +
	// /etc/subgid + newuidmap). Nil ⇒ subuidProvisioned.
	subuidOKFunc func() bool
	// imageLabelsFunc reads an image's OCI labels. nil uses realImageLabels.
	// Split from bakedUIDFunc because it answers WITHOUT starting a container,
	// which is what lets the check complete on a 1.3GB image (CE issue 59).
	imageLabelsFunc func(ctx context.Context, image string) (map[string]string, error)

	// The seams below back checkImageFreshness (see image_freshness.go).
	//
	// imageProber resolves manifest conditions (which optional stacks this
	// host intends to run). Nil ⇒ hostprobe.HostProber.
	imageProber imagemanifest.Prober
	// imageRevisionFunc reads an image's build-revision label, reporting
	// whether the label was present. Nil ⇒ realImageRevision.
	imageRevisionFunc func(ctx context.Context, image string) (revision string, labelled bool, err error)
	// imageRecordFunc loads the release image record — what this release
	// DECLARES its images to be. Injected so the six-scenario truth table
	// (design §10) is unit-testable without a packaged host.
	imageRecordFunc func() (*imagemanifest.ReleaseRecord, error)
	// imageDigestFunc reads an image's manifest digest. Separate from
	// imageRevisionFunc because the two answer different questions: the label
	// says which SOURCE an image came from, the digest says which BUILD.
	imageDigestFunc func(ctx context.Context, image string) (string, error)
	// publishedDigestsFunc reads the per-architecture manifest digests a
	// registry currently serves for a tag, WITHOUT pulling. Nil ⇒ the
	// imagemanifest SkopeoIndexReader the release recorder already uses.
	publishedDigestsFunc func(ctx context.Context, tag string) (map[string]string, error)
	// daemonRevisionFunc reports the commit the DAEMON was built from. The
	// checks run in vornikctl, whose own build may differ, so the revision
	// comes from the daemon's doctor report (New). Nil ⇒ unknown.
	daemonRevisionFunc func() (string, bool)

	// sandboxEnv enables checkSandboxTools (WithSandbox); sandboxProbeFunc
	// is its seam. Nil ⇒ realSandboxProbe, which runs the tools in the
	// pinned agent image.
	sandboxEnv       *SandboxEnv
	sandboxProbeFunc func(ctx context.Context, env SandboxEnv) sandboxOutcome
}

// New builds a Checker for the host. daemonRevision is the daemon's build
// revision as its doctor report states it ("" when the report did not say,
// which makes the freshness check report NOT VERIFIED rather than compare
// against vornikctl's own build).
func New(configDir, usernsMode, daemonRevision string) *Checker {
	return &Checker{
		configDir:  configDir,
		usernsMode: usernsMode,
		daemonRevisionFunc: func() (string, bool) {
			return daemonRevision, daemonRevision != ""
		},
	}
}

// Run executes every host check, in the order the daemon's doctor used to.
func (h *Checker) Run(ctx context.Context) []Check {
	return []Check{
		h.checkPodmanConfig(ctx),
		h.checkAgentImages(ctx),
		h.checkAgentImageUID(ctx),
		h.checkImageFreshness(ctx),
		h.checkSandboxTools(ctx),
	}
}
