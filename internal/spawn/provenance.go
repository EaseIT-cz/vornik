package spawn

// Provenance clones (GitHub #506; design 22, "Amendment 2026-10-22 (#506)",
// C1; process-spawn-law design, "Amendment 2026-10-22 (#506)"). The memory
// provenance verifier reads a daemon-only clone of each repository a project
// ingests from. A provenance root is deliberately NOT a workspace root:
// GitWorkspace and GitHTTPBackend (the daemon's git serving path for agents)
// never run under it, so no agent route can write the objects the verifier
// trusts. Under it the daemon runs only the verifier's reads, a fetch of one
// branch into its tracking ref, and a blob-less clone over https.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// KindGitProvenance is git run in a daemon-only provenance clone.
const KindGitProvenance Kind = "GitProvenance"

var (
	provRootsMu sync.RWMutex
	provRoots   []string
)

// RegisterProvenanceRoot adds a directory under which GitProvenance and
// GitProvenanceClone may run. It is never a workspace root.
func RegisterProvenanceRoot(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("spawn: empty provenance root")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("spawn: provenance root %q: %w", dir, err)
	}
	if abs == string(filepath.Separator) {
		return errors.New("spawn: / is not a provenance root")
	}
	provRootsMu.Lock()
	defer provRootsMu.Unlock()
	for _, r := range provRoots {
		if r == abs {
			return nil
		}
	}
	provRoots = append(provRoots, abs)
	return nil
}

// UnderProvenanceRoot reports whether dir is, or lies beneath, a registered
// provenance root (symlinks followed).
func UnderProvenanceRoot(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	d := resolved(abs)
	provRootsMu.RLock()
	defer provRootsMu.RUnlock()
	for _, r := range provRoots {
		if within(d, resolved(r)) {
			return true
		}
	}
	return false
}

func isProvenanceRoot(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	provRootsMu.RLock()
	defer provRootsMu.RUnlock()
	for _, r := range provRoots {
		if samePath(abs, r) {
			return true
		}
	}
	return false
}

// gitProvenanceSubcommands are the verifier's reads and the sweep's fetch.
var gitProvenanceSubcommands = set("cat-file", "fetch", "remote", "rev-list", "rev-parse")

// fetchRefspec is the only refspec a provenance fetch may name: one branch
// into its origin tracking ref.
var fetchRefspec = regexp.MustCompile(`^\+refs/heads/([A-Za-z0-9._/-]+):refs/remotes/origin/([A-Za-z0-9._/-]+)$`)

// provenanceEnv is the daemon's pinned git environment, https only, with no
// credential prompt.
func provenanceEnv() []string {
	return append(daemonGitEnv(GitDirs{}), "GIT_ALLOW_PROTOCOL=https", "GIT_TERMINAL_PROMPT=0")
}

// GitProvenance accepts `git -C <dir> <subcommand> args…` with dir under a
// registered provenance root, for the verifier's reads and the sweep's fetch.
// No -c option is accepted; remote may only get-url; fetch may only fetch
// origin's one branch into its tracking ref.
func GitProvenance(ctx context.Context, dir string, args ...string) (*Cmd, error) {
	if !UnderProvenanceRoot(dir) {
		return nil, refuse(KindGitProvenance, "directory %q is not under a registered provenance root", dir)
	}
	if len(args) == 0 {
		return nil, refuse(KindGitProvenance, "no subcommand")
	}
	sub, rest := args[0], args[1:]
	if !gitProvenanceSubcommands[sub] {
		return nil, refuse(KindGitProvenance, "subcommand %q is not a provenance read", sub)
	}
	for _, a := range rest {
		for _, o := range gitProgramOptions {
			if a == o || strings.HasPrefix(a, o+"=") {
				return nil, refuse(KindGitProvenance, "option %q names a program", o)
			}
		}
	}
	switch sub {
	case "remote":
		if len(rest) != 2 || rest[0] != "get-url" || rest[1] != "origin" {
			return nil, refuse(KindGitProvenance, "remote may only get-url origin")
		}
	case "fetch":
		ok := len(rest) == 3 && rest[0] == "--quiet" && rest[1] == "origin"
		if ok {
			m := fetchRefspec.FindStringSubmatch(rest[2])
			ok = m != nil && m[1] == m[2] && !strings.Contains(m[1], "..")
		}
		if !ok {
			return nil, refuse(KindGitProvenance, "fetch may only fetch one origin branch into its tracking ref")
		}
	}
	c := newCmd(ctx, KindGitProvenance, "git", append([]string{"-C", dir}, args...))
	c.gitSub = sub
	c.c.Env = provenanceEnv()
	return c, nil
}

// GitProvenanceClone accepts `git clone --quiet --no-checkout
// --filter=blob:none --no-tags -- <url> <dest>` run in root, a registered
// provenance root. url must be plain https (no userinfo, port, query or
// fragment); dest must lie under root and not exist yet.
func GitProvenanceClone(ctx context.Context, root, rawURL, dest string) (*Cmd, error) {
	if !isProvenanceRoot(root) {
		return nil, refuse(KindGitProvenance, "%q is not a registered provenance root", root)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || !plainPort(u.Port()) || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, refuse(KindGitProvenance, "clone URL %q is not a plain https URL", rawURL)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, refuse(KindGitProvenance, "root %q: %v", root, err)
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return nil, refuse(KindGitProvenance, "destination %q: %v", dest, err)
	}
	if absDest == absRoot || !within(absDest, absRoot) ||
		!within(resolved(filepath.Dir(absDest)), resolved(absRoot)) {
		// The parent is resolved too, so a symlinked directory under the
		// root cannot send the clone elsewhere (code review fbe6 F7).
		return nil, refuse(KindGitProvenance, "destination %q is not under the provenance root", dest)
	}
	if _, err := os.Lstat(absDest); err == nil {
		return nil, refuse(KindGitProvenance, "destination %q already exists", dest)
	}
	c := newCmd(ctx, KindGitProvenance, "git", []string{"clone", "--quiet", "--no-checkout",
		"--filter=blob:none", "--no-tags", "--", rawURL, absDest})
	c.c.Dir = absRoot
	c.gitSub = "clone"
	c.c.Env = provenanceEnv()
	return c, nil
}

// plainPort accepts no port, or a numeric port 1-65535: a self-hosted forge
// served on an https port (GitHub #508 F7; process-spawn-law design,
// "Amendment 2026-10-24 (#508)"). The scheme stays https only.
func plainPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == p
}
