package hostdoctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vornik.io/vornik/internal/agentuser"
	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/registry"
)

// agentUIDAgnosticLabel is stamped by images/vornik-agent/Containerfile on any
// image whose writable paths (HOME, the Go cache tree, the contract mount
// points) are usable by an arbitrary uid (D4). Since D5 the key lives in
// internal/agentuser, shared with the runtime, which tests it the same way
// (presence-based, R5).
const agentUIDAgnosticLabel = agentuser.UIDAgnosticLabel

// bakedUIDProbeTimeout bounds the container probes below. It was 5s, which is
// SHORTER THAN THE PROBE: CE issue 59 measured 5.92s / 7.76s / 12.55s across
// three consecutive runs on a 1.3GB image, so the probe was SIGKILLed and the
// check reported "could not read agent image uid: signal: killed" on every run
// of a healthy deployment. 30s is the measured worst case with headroom.
const bakedUIDProbeTimeout = 30 * time.Second

// RuntimeEnv is what checkAgentImageUID needs from the daemon config beyond
// userns_mode (D5 R6): who the agent is configured to run as, and where the
// mounts it reads live.
type RuntimeEnv struct {
	RunAsUser            string
	ProjectWorkspacePath string
	DependencyCacheDir   string
}

// WithRuntime gives the agent_image_uid check the daemon's runtime config.
// Without it run_as_user reads as empty and no mount owner is examined.
func (h *Checker) WithRuntime(env RuntimeEnv) *Checker {
	h.runAsUser = strings.TrimSpace(env.RunAsUser)
	h.workspacesRoot = env.ProjectWorkspacePath
	h.depsCacheDir = env.DependencyCacheDir
	return h
}

// realImageLabels reads an image's labels without starting a container —
// ~55ms against the probe's 5.92-12.55s cold.
func realImageLabels(ctx context.Context, image string) (map[string]string, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "podman", "image", "inspect", image,
		"--format", "{{range $k, $v := .Labels}}{{$k}}={{$v}}\n{{end}}").Output()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" {
			labels[k] = v
		}
	}
	return labels, nil
}

// realBakedUID runs `id -u` inside the agent image. The Containerfile sets
// USER vornik:vornik — a NAME, not a numeric uid — so `podman image
// inspect` cannot yield the baked uid; running id -u inside the image
// returns the effective uid the workload actually runs as. See design F3b.
func realBakedUID(ctx context.Context, image string) (int, error) {
	c, cancel := context.WithTimeout(ctx, bakedUIDProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(c, "podman", "run", "--rm", "--entrypoint", "id", image, "-u").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// labelProbeScript is what the label probe runs inside the image: read the
// contract the way the entrypoint's preflight does (a real read, not test -r),
// then create and remove a file in each of the image's own writable paths.
// Exit 3 is "the image does not work as this uid"; anything else non-zero is
// the probe failing to run.
const labelProbeScript = `head -c1 /app/input/task.json >/dev/null 2>&1 || { echo "cannot read the contract as uid $(id -u)"; exit 3; }
for d in "$HOME" "${GOPATH:-}" "${GOCACHE:-}" "${GOMODCACHE:-}"; do
  [ -n "$d" ] || continue
  mkdir -p "$d" 2>/dev/null
  p="$d/.vornik-doctor.$$"
  ( : > "$p" ) 2>/dev/null || { echo "cannot write in $d as uid $(id -u)"; exit 3; }
  rm -f "$p" 2>/dev/null
done`

// realLabelProbe runs the image as user under keep-id against a temporary 0700
// directory owned by the daemon, mounted where the contract is (D5 R6): the
// label is a claim, this is the measurement.
func realLabelProbe(ctx context.Context, image, runUser string) (bool, string, error) {
	dir, err := os.MkdirTemp("", "vornik-doctor-uid-")
	if err != nil {
		return false, "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Chmod(dir, 0o700); err != nil {
		return false, "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte("{}\n"), 0o600); err != nil {
		return false, "", err
	}
	c, cancel := context.WithTimeout(ctx, bakedUIDProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(c, "podman", "run", "--rm",
		"--userns", "keep-id", "--user", runUser,
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--volume", dir+":/app/input:ro,Z",
		"--entrypoint", "/bin/sh", image, "-c", labelProbeScript).CombinedOutput()
	if err == nil {
		return true, "", nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return false, strings.TrimSpace(string(out)), nil
	}
	return false, "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}

// realDefaultNSStart starts a throwaway container in the default user
// namespace, as the runtime's first attempt does when userns_mode is unset.
// The entrypoint is replaced by `true` so the agent itself never runs.
func realDefaultNSStart(ctx context.Context, image string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, bakedUIDProbeTimeout)
	defer cancel()
	return exec.CommandContext(c, "podman", "run", "--rm", "--entrypoint", "true", image).CombinedOutput()
}

func realOwnerUID(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no owner information for %s", path)
	}
	return int(st.Uid), nil
}

func realHostIdentity() (int, int, bool) {
	return os.Getuid(), os.Getgid(), os.Geteuid() != 0
}

// subuidProvisioned reports whether the current user has /etc/subuid and
// /etc/subgid entries and newuidmap is on PATH — the prerequisites
// userns_mode=keep-id needs to actually remap the container's uid. Without
// these, podman fails to start keep-id containers with an inscrutable error
// rather than a diagnosable one; this is the preflight half of F3b.
func subuidProvisioned() bool {
	if _, err := exec.LookPath("newuidmap"); err != nil {
		return false
	}
	u, err := user.Current()
	if err != nil {
		return false
	}
	has := func(path string) bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, u.Username+":") || strings.HasPrefix(line, u.Uid+":") {
				return true
			}
		}
		return false
	}
	return has("/etc/subuid") && has("/etc/subgid")
}

// checkAgentImageUID reports which user the agent containers will run as and
// whether that user can use both the image and the host's bind mounts
// (onboarding-hardening-design.md, D5 R6). It resolves the user with the
// runtime's own resolver (internal/agentuser), so the doctor and the launch
// cannot disagree about the rule.
//
// D5's incident: with run_as_user empty under keep-id, the published uid-1000
// image ran as 1000 on a uid-1001 host and could not read the daemon's 0700
// contract mount, while this check said OK because the image carries the
// uid-agnostic label. The label is now a claim the check PROBES (R4: a lying
// label is the one case where the runtime's trust in it can turn a success
// into a failure), never a verdict on its own.
//
// Every podman call and host read is a seam (nil ⇒ the real one), so unit
// tests never shell out to podman or touch /etc/subuid.
func (h *Checker) checkAgentImageUID(ctx context.Context) Check {
	name := "agent_image_uid"

	// keep-id preflight runs FIRST and unconditionally — it is a host-level
	// concern (does this machine have the subuid/subgid/newuidmap prereqs
	// keep-id needs?) independent of whether any config directory or agent
	// image is configured yet. Gating it behind the configDir=="" guard
	// below would let a keep-id host with no configDir report a false OK.
	if h.usernsMode == "keep-id" {
		subuidOK := h.subuidOKFunc
		if subuidOK == nil {
			subuidOK = subuidProvisioned
		}
		if !subuidOK() {
			return Check{
				Name:   name,
				Status: "ERROR",
				Message: "userns_mode=keep-id but /etc/subuid|/etc/subgid entries or newuidmap are missing; " +
					"agent containers will fail to start. Fix: `sudo usermod --add-subuids 100000-165535 " +
					"--add-subgids 100000-165535 $USER` then `podman system migrate`.",
			}
		}
	}

	if h.configDir == "" {
		return Check{Name: name, Status: "SKIPPED", Message: "no config directory, skipping"}
	}

	// Image resolution runs unconditionally — in production AND in tests —
	// so the SKIPPED branch is reachable and assertable regardless of
	// whether bakedUIDFunc is injected.
	image, err := firstAgentImage(h.configDir)
	if err != nil {
		return Check{Name: name, Status: "WARNING", Message: "could not resolve agent image: " + err.Error()}
	}
	if image == "" {
		return Check{Name: name, Status: "SKIPPED", Message: "no agent image configured"}
	}

	identity := h.hostIdentityFunc
	if identity == nil {
		identity = realHostIdentity
	}
	uid, gid, rootless := identity()

	verdict, runUID, known := h.agentUIDVerdict(ctx, image, uid, gid, rootless)
	verdict.Name = name
	if known {
		h.addMountOwnerFindings(&verdict, runUID)
	}
	return verdict
}

// agentUIDVerdict is R6's branch table. It returns the verdict and, when the
// host uid the agent will run as is known, that uid for the mount-owner check.
func (h *Checker) agentUIDVerdict(ctx context.Context, image string, uid, gid int, rootless bool) (Check, int, bool) {
	// Row 1: a configured run_as_user is what runs, whatever else holds.
	if runUser, why := agentuser.Resolve(h.runAsUser, "", rootless, false, uid, gid); why == agentuser.ReasonConfigured {
		// The configured user is the operator's lever and is trusted: say so,
		// rather than let the OK read as a probe result (review 539e F1).
		c := Check{Status: "OK", Message: "agent runs as " + runUser + " (configured; the image was not probed as that user)"}
		runUID, err := userUID(runUser)
		if err != nil {
			c.Message += "; mount owners NOT EXAMINED (cannot resolve run_as_user " + runUser + " to a uid: " + err.Error() + ")"
			return c, 0, false
		}
		return c, runUID, true
	}

	userns := strings.ToLower(strings.TrimSpace(h.usernsMode))
	switch {
	case !rootless:
		return sharedNotKeepIDWarning("rootful podman"), 0, false
	case userns == "host":
		return sharedNotKeepIDWarning("userns_mode: host"), 0, false
	case userns == "private":
		// A configured private namespace is the default namespace with no
		// fallback chain: the same subordinate-uid mapping, so the same ERROR.
		c := defaultNamespaceError("userns_mode: private")
		c.Message += " (Reasoned from the measured default namespace, which maps uids the same way; not measured for private. Review 539e F3.)"
		return c, 0, false
	case userns == "":
		// R6 (review 4ab9 F2): decide which attempt the runtime will use by
		// starting a throwaway container in the default namespace, as its
		// first attempt does. Only a namespace-setup failure moves the
		// runtime's chain on to keep-id.
		start := h.defaultNSStartFunc
		if start == nil {
			start = realDefaultNSStart
		}
		out, err := start(ctx, image)
		if err == nil {
			return defaultNamespaceError("userns_mode unset and the default user namespace starts"), 0, false
		}
		if !agentuser.IsUserNSSetupError(out) {
			return Check{Status: "SKIPPED", Message: "which user namespace the agent gets was NOT MEASURED: the default-namespace test start failed (" +
				strings.TrimSpace(err.Error()+" "+string(out)) + ") — this says nothing about the image, only that the probe could not complete."}, 0, false
		}
		// Falls through to keep-id: the chain's keep-id attempt.
	}

	return h.keepIDVerdict(ctx, image, uid, gid)
}

// keepIDVerdict covers a rootless keep-id attempt (configured, or the chain's)
// with run_as_user empty: the label path, probed, or the baked uid.
func (h *Checker) keepIDVerdict(ctx context.Context, image string, uid, gid int) (Check, int, bool) {
	labels := h.imageLabelsFunc
	if labels == nil {
		labels = realImageLabels
	}
	got, lErr := labels(ctx, image)
	agnostic := lErr == nil && agentuser.UIDAgnostic(got)
	runUser, why := agentuser.Resolve("", "keep-id", true, agnostic, uid, gid)

	if why == agentuser.ReasonLabel {
		probe := h.labelProbeFunc
		if probe == nil {
			probe = realLabelProbe
		}
		works, detail, err := probe(ctx, image, runUser)
		if err != nil {
			return Check{Status: "SKIPPED", Message: "agent image NOT MEASURED as uid " + strconv.Itoa(uid) + " (" + err.Error() +
				") — this says nothing about the image, only that the probe could not complete."}, 0, false
		}
		if !works {
			return Check{Status: "ERROR", Message: "the image claims uid-agnostic but does not work as " +
				strconv.Itoa(uid) + " (" + detail + "; label " + agentUIDAgnosticLabel + "); the agent runs as " + runUser + " here because runtime.run_as_user is empty. " +
				"Workaround: set `runtime.run_as_user` to the image's baked uid:gid, or rebuild with `make build-agent`."}, 0, false
		}
		return Check{Status: "OK", Message: "agent runs as " + strconv.Itoa(uid) + " (the image is uid-agnostic, verified)"}, uid, true
	}

	// Unlabelled: the baked uid runs, read with the slow probe (up to its 30s
	// deadline, as D3 already pays for unlabelled images).
	baked := h.bakedUIDFunc
	if baked == nil {
		baked = realBakedUID
	}
	bakedUID, err := baked(ctx, image)
	if err != nil {
		// SKIPPED, not WARNING: 2026-08-26-doctor-skipped-vs-ok-design.md §E2
		// "the check could not run at all → SKIPPED … never WARNING. A driver
		// error is never a verdict" (CE issue 59).
		return Check{
			Status: "SKIPPED",
			Message: "agent image uid NOT MEASURED (" + err.Error() + ") — this says nothing about the image, only that the probe could not complete. " +
				"An image carrying the " + agentUIDAgnosticLabel + " label needs no uid probe; re-pull or rebuild the agent image to get one.",
		}, 0, false
	}
	if bakedUID == uid {
		return Check{Status: "OK", Message: "agent runs as the image's baked uid " + strconv.Itoa(bakedUID) + ", which matches the daemon's (keep-id maps it to the workspace owner)"}, uid, true
	}
	return Check{
		Status: "ERROR",
		Message: "agent image built for uid " + strconv.Itoa(bakedUID) + " but the daemon runs as uid " + strconv.Itoa(uid) +
			", it carries no " + agentUIDAgnosticLabel + " label, and runtime.run_as_user is empty, so the agent runs as " + strconv.Itoa(bakedUID) +
			" and cannot use the daemon's 0700 mounts. Fix: set `runtime.run_as_user: \"" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid) +
			"\"`, or re-pull the agent image (a uid-agnostic one), or rebuild with `make build-agent` — never a bare `podman build`.",
	}, 0, false
}

// sharedNotKeepIDWarning is R6's one message for rootful podman and for
// userns_mode: host (review 4ab9 F4): the keep-id rule does not apply, and the
// baked uid meets the daemon's 0700 mounts as a different user.
func sharedNotKeepIDWarning(what string) Check {
	return Check{Status: "WARNING", Message: what + ": set `runtime.run_as_user`; the keep-id rule does not apply"}
}

// defaultNamespaceError is R6's ERROR for the default namespace: the non-root
// agent's uid maps to a subordinate uid, which cannot read the daemon's 0700
// mounts (measured 2026-10-02: `default | 1001:1001` → denied).
func defaultNamespaceError(what string) Check {
	return Check{Status: "ERROR", Message: what + ": the agent's uid maps to a subordinate uid that cannot read the daemon's 0700 contract mounts. " +
		"Fix: set `runtime.userns_mode: keep-id`."}
}

// userUID resolves a run_as_user value ("uid", "uid:gid", "user:group") to
// the uid it names.
func userUID(runUser string) (int, error) {
	name, _, _ := strings.Cut(runUser, ":")
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

// addMountOwnerFindings is R6's mount-owner branch (review 44bf F2, 4ab9 F3):
// for each configured project, the owner uid of its project directory, its git
// directory and each dependency mount's host path is compared with the uid the
// agent runs as. A mismatch is a WARNING naming the project and the path —
// group or ACL access may still work, and the doctor cannot tell without
// running the agent. The message states how many paths were examined, so
// "no mismatch" is distinguishable from "nothing examined".
func (h *Checker) addMountOwnerFindings(c *Check, runUID int) {
	if h.workspacesRoot == "" {
		c.Message += "; mount owners NOT EXAMINED (no project workspace path configured)"
		return
	}
	projects, err := registry.LoadProjects(h.configDir)
	if err != nil {
		c.Message += "; mount owners NOT EXAMINED (cannot load projects: " + err.Error() + ")"
		return
	}
	owner := h.ownerUIDFunc
	if owner == nil {
		owner = realOwnerUID
	}
	ids := make([]string, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	examined, mismatched := 0, 0
	for _, id := range ids {
		projDir := filepath.Join(h.workspacesRoot, id)
		paths := []string{projDir, filepath.Join(projDir, ".git")}
		paths = append(paths, h.dependencyMountPaths(projects[id], projDir)...)
		for _, p := range paths {
			got, err := owner(p)
			if err != nil {
				// An absent path is not a mount yet (a project never run, a
				// non-git project); it is not counted as examined.
				continue
			}
			examined++
			if got != runUID {
				mismatched++
				c.Items = append(c.Items, fmt.Sprintf("project %s: %s is owned by uid %d, the agent runs as uid %d", id, p, got, runUID))
			}
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	}
	c.Message += fmt.Sprintf("; %d %s examined across %d %s, %d owned by another uid",
		examined, plural(examined, "mount path", "mount paths"), len(ids), plural(len(ids), "project", "projects"), mismatched)
	if mismatched > 0 && c.Status == "OK" {
		c.Status = "WARNING"
		c.Message += " (group or ACL access may still work; the doctor cannot tell without running the agent)"
	}
}

// dependencyMountPaths returns the host paths of a project's materialised
// dependency mounts, planned the way the executor plans them
// (projectdeps.Resolver over the dependency cache).
func (h *Checker) dependencyMountPaths(p *registry.Project, projDir string) []string {
	if h.dependencyMountsFunc != nil {
		return h.dependencyMountsFunc(p.ID, projDir)
	}
	if h.depsCacheDir == "" || len(p.Dependencies) == 0 {
		return nil
	}
	store := projectdeps.NewStore(h.depsCacheDir)
	var out []string
	for _, plan := range projectdeps.NewResolver(store, "").Plan(projDir, p.Dependencies) {
		if plan.Problem == nil && plan.Materialised {
			out = append(out, plan.Mount(store).HostPath)
		}
	}
	return out
}
