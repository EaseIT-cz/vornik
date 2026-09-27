// Package install materialises project dependency trees. It is run ONLY by
// `vornikctl deps install` (project dependency provisioning design §8): the
// daemon must never fetch or install, because what would trigger it is a
// project config editable through the control plane, and the 2026-08-03 ruling
// forbids any such path from spawning a process. internal/architecture pins
// that no daemon main links this package.
//
// Everything runs INSIDE the agent image, through podman, so the tree matches
// the interpreter the agents run: the host's Python is not theirs (3.14 against
// 3.12 on the reference host, 2026-09-25).
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"vornik.io/vornik/internal/projectdeps"
)

// Runner runs one program and returns its combined output. The seam every
// podman call goes through, so tests need no podman.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Fetcher populates dir, a STAGING directory Materialise created, with the
// ecosystem's tree. A failure leaves the cache untouched.
type Fetcher func(ctx context.Context, dir string) error

// probeScript prints the interpreter triple: cache tag, machine, libc. Two
// images that agree on cpython-312 but differ in architecture or libc are
// different interpreters for a compiled wheel.
const probeScript = `import glob, os, platform, sys; l = platform.libc_ver(); m = glob.glob("/lib/ld-musl-*.so.1"); libc = (l[0] + "-" + l[1]) if l[0] else (("musl-" + os.path.basename(m[0])) if m else "unknown-unknown"); print(sys.implementation.cache_tag, platform.machine(), libc)`

// imageRefPattern is what an image reference may look like: registry, path,
// tag and digest characters, and it must start alphanumeric.
var imageRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$`)

// ValidImageRef refuses an image reference podman could read as something
// else. The reference comes from a control-plane-editable config and is passed
// as a bare argument, so "--privileged" or "-v /:/host" must never reach
// podman's option parser.
func ValidImageRef(image string) error {
	if !imageRefPattern.MatchString(image) {
		return fmt.Errorf("image reference %q is not a valid image reference", image)
	}
	return nil
}

// ImageID returns the local image ID of image, or an error when the image is
// not present on this host. It never pulls: the image reference comes from a
// control-plane-editable config, so fetching whatever it names would let a
// config choose code for the operator to run (§8.1 step 2).
func ImageID(ctx context.Context, run Runner, image string) (string, error) {
	if err := ValidImageRef(image); err != nil {
		return "", err
	}
	out, err := run(ctx, "podman", "image", "inspect", "--format", "{{.Id}}", "--", image)
	if err != nil {
		return "", fmt.Errorf("image %s is not present on this host (it is never pulled): %w", image, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("image %s: podman returned no image id", image)
	}
	return id, nil
}

// Probe returns image's interpreter triple, refusing an image whose python3
// does not run (a config can name any local image).
func Probe(ctx context.Context, run Runner, image string) (string, error) {
	if err := ValidImageRef(image); err != nil {
		return "", err
	}
	out, err := run(ctx, "podman", "run", "--rm", "--pull=never", "--network=none",
		"--entrypoint", "python3", "--", image, "-c", probeScript)
	if err != nil {
		return "", fmt.Errorf("image %s has no usable python3 (probe failed: %v): %s", image, err, tailLines(string(out), 5))
	}
	triple := strings.TrimSpace(string(out))
	if len(strings.Fields(triple)) != 3 {
		return "", fmt.Errorf("image %s: probe printed %q, not an interpreter triple", image, triple)
	}
	return triple, nil
}

// ChooseInterpreter probes every image and returns their common triple, or
// refuses naming each image and its triple: one tree serves every role
// (§5.4), so it can be right for only one interpreter.
func ChooseInterpreter(ctx context.Context, run Runner, images []string) (string, error) {
	if len(images) == 0 {
		return "", errors.New("no role images to install for")
	}
	seen := map[string][]string{}
	for _, img := range images {
		triple, err := Probe(ctx, run, img)
		if err != nil {
			return "", err
		}
		seen[triple] = append(seen[triple], img)
	}
	if len(seen) == 1 {
		for triple := range seen {
			return triple, nil
		}
	}
	parts := make([]string, 0, len(seen))
	for triple, imgs := range seen {
		parts = append(parts, fmt.Sprintf("%s → %s", strings.Join(imgs, ", "), triple))
	}
	sort.Strings(parts)
	return "", fmt.Errorf("the project's role images run different Pythons, and one dependency tree serves every role: %s", strings.Join(parts, "; "))
}

// PipFetcher installs a hash-pinned lockfile into the staging directory,
// inside image. wheelhouse "" is a connected install, on the image's default
// network (pip fetches from the index INSIDE the container); a directory is an
// air-gapped install from it, with no network at all.
//
// --only-binary=:all: means no build backend ever runs: a lockfile that needs
// an sdist is refused with pip's own message. --require-hashes WITHOUT
// --no-deps: pip then refuses any dependency the lockfile does not pin with a
// hash, so an incomplete lockfile fails loudly instead of installing a tree
// that imports half of what it needs (review-20260925-0325 F8), and pip still
// cannot install anything the file does not name. --userns=keep-id so the tree
// is the invoking user's, the daemon's user, which must mount it.
//
// Only the lockfile FILE is mounted, not its directory: the directory is the
// project checkout, and a connected install has network (review F2).
func PipFetcher(run Runner, image, lockfilePath, wheelhouse string) Fetcher {
	return func(ctx context.Context, dir string) error {
		if err := ValidImageRef(image); err != nil {
			return err
		}
		for _, src := range []string{lockfilePath, dir, wheelhouse} {
			if src == "" {
				continue
			}
			if err := validMountSource(src); err != nil {
				return err
			}
		}
		args := []string{"run", "--rm", "--pull=never", "--userns=keep-id"}
		if wheelhouse != "" {
			args = append(args, "--network=none", "-v", wheelhouse+":/wheels:ro,z")
		}
		args = append(args,
			// ,z: the shared SELinux label every agent mount already uses
			// (runtime/manager.go); without it an enforcing host denies the
			// container the read, as the podman integration test found.
			"-v", lockfilePath+":/lock/requirements.lock:ro,z",
			"-v", dir+":/staging:rw,z",
			"--entrypoint", "python3", "--", image, "-m", "pip", "install",
			"--require-hashes", "--only-binary=:all:",
			"--no-input", "--disable-pip-version-check",
			"--target", "/staging/"+projectdeps.SiteDir,
			"--requirement", "/lock/requirements.lock",
		)
		if wheelhouse != "" {
			args = append(args, "--no-index", "--find-links", "/wheels")
		}
		if out, err := run(ctx, "podman", args...); err != nil {
			return fmt.Errorf("pip install in %s failed: %w: %s", image, err, tailLines(string(out), 40))
		}
		return nil
	}
}

// validMountSource refuses a bind-mount source podman could misread: it must
// be an absolute, clean path that does not start with "-" and carries no ":"
// or "," (the -v field separators).
func validMountSource(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ":,\n") {
		return fmt.Errorf("mount source %q must be an absolute, clean path without ':' or ','", p)
	}
	return nil
}

// ConfinedLockfile refuses a lockfile path outside the project root. The
// planner already confines it (resolveLockfilePath, design §7a item 2); the
// installer checks again because the path becomes a bind-mount source.
func ConfinedLockfile(projectRoot, lockfilePath string) error {
	rel, err := filepath.Rel(projectRoot, lockfilePath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("lockfile %s is not inside the project root %s", lockfilePath, projectRoot)
	}
	return nil
}

// WheelhouseCommand is what the operator runs on a CONNECTED host to build the
// wheelhouse for an air-gapped `deps install --from`: pip download in the same
// image, so no wheel tag has to be computed by hand. Run from a writable
// directory. Every config-derived token is shell-quoted: the command is meant
// to be pasted, and a lockfile path or image reference is config (review F4).
func WheelhouseCommand(image, lockfilePath string) string {
	return "podman run --rm --userns=keep-id -v " + shellQuote(lockfilePath+":/lock/requirements.lock:ro,z") +
		` -v "$PWD/wheelhouse":/wheelhouse:rw,z --entrypoint python3 -- ` + shellQuote(image) +
		" -m pip download --require-hashes --only-binary=:all: -r /lock/requirements.lock -d /wheelhouse"
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Materialise installs key into store unless it is already complete: staging
// directory, fetch, sitecustomize.py, completion marker (with meta), a pass
// making the whole tree world-readable (marker included), atomic rename. The
// tree is package code, not secrets, and the agent container reads it through
// a read-only mount whose uid may not be the host user's.
func Materialise(ctx context.Context, store *projectdeps.Store, key string, fetch Fetcher, meta projectdeps.MarkerMeta) (string, error) {
	target := store.Path(key)
	done, err := store.IsMaterialised(key)
	if err != nil {
		return "", err
	}
	if done {
		return target, nil
	}
	// Present but unmarked: not a state this code produces. Refuse and name
	// the path rather than deleting it.
	if _, statErr := os.Stat(target); statErr == nil {
		return "", fmt.Errorf("%w: %s — inspect it and remove it by hand if it is stale", projectdeps.ErrCorruptMaterialisation, target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", target, statErr)
	}
	if err := os.MkdirAll(store.Root(), 0o755); err != nil {
		return "", fmt.Errorf("create deps root %s: %w", store.Root(), err)
	}
	staging, err := os.MkdirTemp(store.Root(), ".staging-")
	if err != nil {
		return "", fmt.Errorf("create staging dir under %s: %w", store.Root(), err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := fetch(ctx, staging); err != nil {
		return "", fmt.Errorf("install %s: %w", key, err)
	}
	if err := projectdeps.WriteSiteCustomise(staging); err != nil {
		return "", err
	}
	meta.Key = key
	if meta.Completed.IsZero() {
		meta.Completed = time.Now().UTC()
	}
	if err := writeMarker(filepath.Join(staging, projectdeps.CompletionMarker), meta); err != nil {
		return "", err
	}
	if err := makeReadable(staging); err != nil {
		return "", fmt.Errorf("make %s readable: %w", key, err)
	}
	if err := os.Rename(staging, target); err != nil {
		// Another installer of the same key won the race; its result is
		// byte-identical by construction (the key IS the content).
		if done, checkErr := store.IsMaterialised(key); checkErr == nil && done {
			return target, nil
		}
		return "", fmt.Errorf("publish %s: %w", key, err)
	}
	if err := fsyncDir(store.Root()); err != nil {
		return "", fmt.Errorf("fsync deps root after publishing %s: %w", key, err)
	}
	return target, nil
}

// writeMarker writes and fsyncs the marker (0644), then fsyncs its directory:
// a marker in the page cache can outlive a crash while its tree does not.
func writeMarker(path string, meta projectdeps.MarkerMeta) error {
	body, err := projectdeps.EncodeMarker(meta)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write completion marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync completion marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close completion marker: %w", err)
	}
	return fsyncDir(filepath.Dir(path))
}

// makeReadable sets directories 0755, executable files 0755 (console
// scripts must stay runnable) and other files 0644, and REFUSES a symlink
// whose target leaves the tree: the tree is bind-mounted into agent
// containers, and an escaping link would resolve against the host
// (review-20260925-0325 F3, F6). A wheel's bytes are hash-pinned, but a
// hash pins bytes, not what they point at.
func makeReadable(dir string) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("%s is a symlink to the absolute path %s, which would resolve outside the mounted tree", p, target)
			}
			rel, rerr := filepath.Rel(dir, filepath.Join(filepath.Dir(p), target))
			if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s is a symlink to %s, which leaves the installed tree", p, target)
			}
			return nil
		case info.IsDir():
			return os.Chmod(p, 0o755)
		case info.Mode().Perm()&0o111 != 0:
			return os.Chmod(p, 0o755)
		default:
			return os.Chmod(p, 0o644)
		}
	})
}

// CheckOwner refuses when the calling user does not own the cache directory:
// a tree written by root is one the daemon's user may be unable to mount. An
// absent directory is fine; Materialise creates it as the caller.
func CheckOwner(root string) error {
	return checkOwnerUID(root, os.Geteuid())
}

func checkOwnerUID(root string, uid int) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Not a Unix stat (podman, and this verb, are Linux-only in practice):
		// ownership cannot be read, so it is not enforced.
		return nil
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d and this is uid %d: run `vornikctl deps install` as the user that runs the daemon", root, st.Uid, uid)
	}
	return nil
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
}
