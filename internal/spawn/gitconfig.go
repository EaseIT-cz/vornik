package spawn

// S6 — git state an agent can write (process-spawn law design, "S6"). The law
// pins WHICH program the daemon runs, but git runs other programs, chosen by
// the state it reads. This file pins WHERE daemon git may read from:
//
//   - S6-D3: a command whose directory is a task worktree
//     (<root>/…/<project>/.worktrees/<name>) must carry exactly
//     GIT_DIR=<project>/.git/worktrees/<name> and
//     GIT_WORK_TREE=<project>/.worktrees/<name>, which override discovery and
//     core.worktree, so the agent-writable .git pointer is never read.
//   - S6-D4: every daemon git command (GitWorkspace and GitHTTPBackend) runs
//     with GIT_CONFIG_NOSYSTEM=1 and GIT_CONFIG_GLOBAL=<composed file>, a file
//     the daemon writes at startup from the operator's system and global
//     config keeping only an ALLOWLIST of keys. Nothing composed means an
//     empty global config (os.DevNull): fail closed.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// GitDirs names the repository a worktree command reads: the worktree's admin
// dir under the project's .git, and the worktree itself. The daemon derives
// both from its own paths, never from the worktree's .git file.
type GitDirs struct {
	GitDir   string
	WorkTree string
}

// GitWorkspaceDirs is GitWorkspace for a command that runs in a task
// worktree: dirs must be exactly the ones the worktree's path implies
// (S6-D3), or the command is refused. dirs must be empty for any other
// directory.
func GitWorkspaceDirs(ctx context.Context, dir string, dirs GitDirs, args ...string) (*Cmd, error) {
	return gitWorkspace(ctx, dir, dirs, args)
}

// worktreeOf reports the task worktree a resolved directory lies in: for
// <root>/<p…>/.worktrees/<name>[/…] it returns the worktree and its admin dir
// (both under the resolved root), taking the FIRST .worktrees segment below
// the root. ok=false for any other directory.
func worktreeOf(d string) (GitDirs, bool) {
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	for _, r := range roots {
		rr := resolved(r)
		if !within(d, rr) {
			continue
		}
		rel, err := filepath.Rel(rr, d)
		if err != nil || rel == "." {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for i := 1; i+1 < len(parts); i++ {
			if parts[i] != ".worktrees" {
				continue
			}
			proj := filepath.Join(append([]string{rr}, parts[:i]...)...)
			name := parts[i+1]
			return GitDirs{
				GitDir:   filepath.Join(proj, ".git", "worktrees", name),
				WorkTree: filepath.Join(proj, ".worktrees", name),
			}, true
		}
	}
	return GitDirs{}, false
}

// samePath compares two paths the way the root check does: absolute, symlinks
// followed where they exist.
func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return resolved(aa) == resolved(bb)
}

// checkGitDirs is S6-D3's judgement for a command whose (resolved) directory
// is d.
func checkGitDirs(d string, dirs GitDirs) error {
	want, isWorktree := worktreeOf(d)
	if !isWorktree {
		if dirs != (GitDirs{}) {
			return refuse(KindGitWorkspace, "GIT_DIR/GIT_WORK_TREE are only for a task worktree, and %q is not one", d)
		}
		return nil
	}
	if !samePath(dirs.GitDir, want.GitDir) {
		return refuse(KindGitWorkspace, "a command in worktree %q must carry GIT_DIR=%s (got %q)", want.WorkTree, want.GitDir, dirs.GitDir)
	}
	if !samePath(dirs.WorkTree, want.WorkTree) {
		return refuse(KindGitWorkspace, "a command in worktree %q must carry GIT_WORK_TREE=%s (got %q)", want.WorkTree, want.WorkTree, dirs.WorkTree)
	}
	return nil
}

// inheritedGitVars are daemon environment variables that would change which
// repository or which config a git command reads. They never pass through to
// daemon git: the repository comes from -C (and, in a worktree, the pinned
// dirs) and the config from the composed file alone.
func inheritedGitVar(key string) bool {
	switch key {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR":
		return true
	}
	return strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_")
}

// daemonGitEnv is the environment of a GitWorkspace command: the daemon's,
// minus every inherited repository/config selector, plus the D4 pair and, in
// a worktree, the D3 pair.
func daemonGitEnv(dirs GitDirs) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if inheritedGitVar(k) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, gitConfigPins()...)
	if dirs != (GitDirs{}) {
		env = append(env, "GIT_DIR="+dirs.GitDir, "GIT_WORK_TREE="+dirs.WorkTree)
	}
	return env
}

var (
	gitGlobalMu   sync.RWMutex
	gitGlobalPath string
)

// GitGlobalConfig is the registered composed global config ("" when none).
func GitGlobalConfig() string {
	gitGlobalMu.RLock()
	defer gitGlobalMu.RUnlock()
	return gitGlobalPath
}

// RegisterGitGlobalConfig makes path the global config every daemon git
// command reads (S6-D4) and returns a func that restores the previous one.
// ComposeGitConfig calls it; tests call it directly.
func RegisterGitGlobalConfig(path string) (restore func()) {
	gitGlobalMu.Lock()
	prev := gitGlobalPath
	gitGlobalPath = path
	gitGlobalMu.Unlock()
	return func() {
		gitGlobalMu.Lock()
		gitGlobalPath = prev
		gitGlobalMu.Unlock()
	}
}

// gitConfigPins is S6-D4's pair. With nothing composed git reads an empty
// global config: an unconfigured daemon inherits nothing, rather than
// everything.
func gitConfigPins() []string {
	global := GitGlobalConfig()
	if global == "" {
		global = os.DevNull
	}
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + global}
}

// --- S6-D4 composition ---

// GitConfigEntry is one key of a git config listing (`git config --list -z`):
// the key as git prints it (section and name lowercased, subsection as
// written), its value, and NoValue for a bare boolean key.
type GitConfigEntry struct {
	Key     string
	Value   string
	NoValue bool
}

// DroppedGitConfigKey is an operator key the composed config does not carry,
// and why. The doctor lists them so an operator is never surprised.
type DroppedGitConfigKey struct {
	Key    string
	Reason string
}

// GitConfigComposition is what ComposeGitConfig wrote. Examined is the
// denominator: every entry read, kept or dropped.
type GitConfigComposition struct {
	Path     string
	Examined int
	Kept     []GitConfigEntry
	Dropped  []DroppedGitConfigKey
}

// splitGitKey splits "section.sub.section.name" into its three parts; the
// subsection is everything between the first and the last dot.
func splitGitKey(key string) (section, sub, name string) {
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 0 {
		return strings.ToLower(key), "", ""
	}
	section = strings.ToLower(key[:first])
	name = strings.ToLower(key[last+1:])
	if last > first {
		sub = key[first+1 : last]
	}
	return section, sub, name
}

// Drop reasons (the principle: an agent must not be able to SELECT a program).
const (
	reasonDriver     = "a filter/diff/merge driver or textconv: named by a work tree's .gitattributes, which an agent writes"
	reasonHooksPath  = "would redirect the daemon's own receive guards away from the project's hook directory"
	reasonAttributes = "an attributes file can name a driver"
	reasonInclude    = "includes are flattened into the composed file; a conditional include depends on the repository and is not inherited"
	reasonNotNeeded  = "a program for interactive git; daemon git is non-interactive"
	reasonSigning    = "the daemon does not sign commits or pushes"
	reasonNotAllowed = "not on the composed config's allowlist"
)

// gitKeyVerdict decides one key: keep, or drop with the reason.
func gitKeyVerdict(key string) (keep bool, reason string) {
	section, sub, name := splitGitKey(key)
	switch section {
	case "user", "credential", "http", "https", "pull":
		return true, ""
	case "init":
		return sub == "" && name == "defaultbranch", reasonNotAllowed
	case "safe":
		return sub == "" && name == "directory", reasonNotAllowed
	case "url":
		return sub != "" && (name == "insteadof" || name == "pushinsteadof"), reasonNotAllowed
	case "ssh":
		return sub == "" && name == "variant", reasonNotAllowed
	case "push":
		return sub == "" && name == "default", reasonNotAllowed
	case "fetch":
		return sub == "" && name == "prune", reasonNotAllowed
	case "filter", "diff", "merge":
		return false, reasonDriver
	case "include", "includeif":
		return false, reasonInclude
	case "gpg":
		return false, reasonSigning
	case "commit", "tag":
		if name == "gpgsign" {
			return false, reasonSigning
		}
		return false, reasonNotAllowed
	case "core":
		if sub != "" {
			return false, reasonNotAllowed
		}
		switch name {
		case "sshcommand", "excludesfile", "autocrlf", "eol", "quotepath":
			return true, ""
		case "hookspath":
			return false, reasonHooksPath
		case "attributesfile":
			return false, reasonAttributes
		case "fsmonitor", "pager", "editor":
			return false, reasonNotNeeded
		}
	}
	if name == "textconv" {
		return false, reasonDriver
	}
	return false, reasonNotAllowed
}

// FilterGitConfig splits entries into the ones the composed config keeps and
// the ones it drops, each with its reason. Order is preserved, so a
// multi-valued key (credential.helper, with an empty value resetting the
// list) keeps its meaning.
func FilterGitConfig(entries []GitConfigEntry) (kept []GitConfigEntry, dropped []DroppedGitConfigKey) {
	for _, e := range entries {
		if ok, reason := gitKeyVerdict(e.Key); ok {
			kept = append(kept, e)
		} else {
			dropped = append(dropped, DroppedGitConfigKey{Key: e.Key, Reason: reason})
		}
	}
	return kept, dropped
}

// ParseGitConfigList parses `git config --list -z`: each entry is the key, a
// newline and the value, NUL-terminated; a bare boolean key has no newline.
func ParseGitConfigList(out []byte) []GitConfigEntry {
	var entries []GitConfigEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		k, v, hasValue := bytes.Cut(rec, []byte{'\n'})
		entries = append(entries, GitConfigEntry{Key: string(k), Value: string(v), NoValue: !hasValue})
	}
	return entries
}

func quoteGitValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(v) + `"`
}

// RenderGitConfig writes entries as a git config file, one section header per
// entry so the order of every key is exactly the input's.
func RenderGitConfig(entries []GitConfigEntry) []byte {
	var b strings.Builder
	b.WriteString("# Composed by the vornik daemon at startup from the operator's system and\n")
	b.WriteString("# global git config, keeping only allowlisted keys (process-spawn law S6-D4).\n")
	b.WriteString("# Do not edit: it is rewritten on every start. `vornikctl doctor` lists what it dropped.\n")
	for _, e := range entries {
		section, sub, name := splitGitKey(e.Key)
		if name == "" {
			continue
		}
		if sub != "" {
			esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(sub)
			fmt.Fprintf(&b, "[%s \"%s\"]\n", section, esc)
		} else {
			fmt.Fprintf(&b, "[%s]\n", section)
		}
		if e.NoValue {
			fmt.Fprintf(&b, "\t%s\n", name)
			continue
		}
		fmt.Fprintf(&b, "\t%s = %s\n", name, quoteGitValue(e.Value))
	}
	return []byte(b.String())
}

// readOperatorGitConfig lists one scope of the operator's config with
// includes flattened. A scope with no file is empty, not an error. It runs in
// "/" so no repository's config or conditional include can apply.
func readOperatorGitConfig(ctx context.Context, scope string) ([]GitConfigEntry, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--"+scope, "--includes", "--list", "-z")
	cmd.Dir = string(filepath.Separator)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		var ee *exec.ExitError
		// Exit 1 with no output: the scope has no keys. A missing file is
		// reported as "unable to read config file … No such file".
		if errors.As(err, &ee) && (strings.Contains(msg, "No such file or directory") || (ee.ExitCode() == 1 && strings.TrimSpace(msg) == "")) {
			return nil, nil
		}
		return nil, fmt.Errorf("git config --%s --list: %w: %s", scope, err, strings.TrimSpace(msg))
	}
	return ParseGitConfigList(out), nil
}

// ComposeGitConfig writes <dataDir>/git/config from the operator's system and
// global git config (in that order, as git reads them), keeping only the
// allowlist (S6-D4), and registers it for every daemon git command. It is the
// daemon's startup step; nothing a request can reach calls it. On an error
// nothing is registered, so daemon git reads an empty global config.
func ComposeGitConfig(ctx context.Context, dataDir string) (GitConfigComposition, error) {
	if strings.TrimSpace(dataDir) == "" {
		return GitConfigComposition{}, errors.New("spawn: compose git config: no data directory")
	}
	var all []GitConfigEntry
	if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		sys, err := readOperatorGitConfig(ctx, "system")
		if err != nil {
			return GitConfigComposition{}, err
		}
		all = append(all, sys...)
	}
	glob, err := readOperatorGitConfig(ctx, "global")
	if err != nil {
		return GitConfigComposition{}, err
	}
	all = append(all, glob...)

	kept, dropped := FilterGitConfig(all)
	dir := filepath.Join(dataDir, "git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return GitConfigComposition{}, fmt.Errorf("spawn: compose git config: %w", err)
	}
	path := filepath.Join(dir, "config")
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return GitConfigComposition{}, fmt.Errorf("spawn: compose git config: %w", err)
	}
	_, werr := tmp.Write(RenderGitConfig(kept))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return GitConfigComposition{}, fmt.Errorf("spawn: compose git config: %w", werr)
	}
	RegisterGitGlobalConfig(path)
	return GitConfigComposition{Path: path, Examined: len(all), Kept: kept, Dropped: dropped}, nil
}
