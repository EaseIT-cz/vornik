// Package agentuser decides which user an agent container runs as. It is the
// ONE resolver the runtime (internal/runtime, per podman run attempt) and the
// doctor (internal/hostdoctor, agent_image_uid) share, so the two cannot drift:
// onboarding-hardening-design.md, D5 R3.
//
// It is a leaf: no podman, no I/O. Callers read the image label and the
// process identity and pass them in.
package agentuser

import (
	"strconv"
	"strings"
)

// UIDAgnosticLabel is stamped by images/vornik-agent/Containerfile on any image
// whose own writable paths (HOME, the Go cache tree, the contract mount points)
// are usable by an arbitrary uid (D4). It is presence-based (D5 R5): the
// Containerfile stamps "1", and any value counts.
const UIDAgnosticLabel = "io.vornik.agent.uid-agnostic"

// Reason names the R3 row that decided the user.
type Reason string

const (
	// ReasonConfigured means runtime.run_as_user is set and is passed as given.
	ReasonConfigured Reason = "configured"
	// ReasonLabel means run_as_user is empty, a rootless keep-id attempt and a
	// uid-agnostic image, so the daemon's uid:gid is synthesised.
	ReasonLabel Reason = "label"
	// ReasonUnlabelled is as ReasonLabel but the image carries no label, so its
	// baked uid may be the only one that works; no --user.
	ReasonUnlabelled Reason = "unlabelled"
	// ReasonNotKeepID means the attempt uses the default, host or private
	// namespace; the keep-id rule does not apply.
	ReasonNotKeepID Reason = "not keep-id"
	// ReasonRootful means podman runs as root; keep-id is not the model there and
	// run_as_user is the lever.
	ReasonRootful Reason = "rootful"
)

// Synthesised reports whether the row puts a --user on the command line that
// the operator did not configure. Only the label row does.
func (r Reason) Synthesised() bool { return r == ReasonLabel }

// Resolve is R3's table: which --user an agent container gets for one podman
// run attempt. userns is that attempt's --userns value ("" for the default
// namespace), so the fallback chain's keep-id attempt resolves exactly as a
// configured keep-id does. An empty user means "no --user": the image's baked
// USER applies.
func Resolve(runAsUser, userns string, rootless, imageUIDAgnostic bool, daemonUID, daemonGID int) (string, Reason) {
	if u := strings.TrimSpace(runAsUser); u != "" {
		return u, ReasonConfigured
	}
	if !rootless {
		return "", ReasonRootful
	}
	if !IsKeepID(userns) {
		return "", ReasonNotKeepID
	}
	if !imageUIDAgnostic {
		return "", ReasonUnlabelled
	}
	return strconv.Itoa(daemonUID) + ":" + strconv.Itoa(daemonGID), ReasonLabel
}

// IsKeepID reports whether a --userns value is keep-id (including the
// keep-id:uid=,gid= form), case-insensitively as config validation accepts it.
func IsKeepID(userns string) bool {
	u := strings.ToLower(strings.TrimSpace(userns))
	return u == "keep-id" || strings.HasPrefix(u, "keep-id:")
}

// UIDAgnostic reports whether an image's labels carry UIDAgnosticLabel,
// whatever its value.
func UIDAgnostic(labels map[string]string) bool {
	_, ok := labels[UIDAgnosticLabel]
	return ok
}

// IsUserNSSetupError reports whether podman's output says it could not set up
// the rootless user namespace (newuidmap/newgidmap, the pause process). This is
// the condition on which the runtime's auto-fallback chain moves from the
// default namespace to keep-id, and on which the doctor concludes the runtime
// would (D5 R1, R6).
func IsUserNSSetupError(output []byte) bool {
	msg := strings.ToLower(string(output))
	return strings.Contains(msg, "newuidmap") ||
		strings.Contains(msg, "newgidmap") ||
		strings.Contains(msg, "unable to create a new pause process") ||
		strings.Contains(msg, "cannot set up namespace")
}
