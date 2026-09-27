// Package spawn is the one package linked into a daemon that may start a
// process (https://docs.vornik.io §3 and
// "S1b-2, as built"). Every other daemon package spawns through it, and it
// accepts only a closed set of kinds:
//
//   - PodmanAgent: `podman run` of the pinned agent image, flags from a closed
//     grammar;
//   - PodmanControl: the daemon's podman verbs (version, system migrate, stop,
//     rm, inspect, ps, wait, logs, image inspect of the pinned image);
//   - GitWorkspace: `git -C <dir>` with dir under a registered workspace root,
//     a closed set of subcommands, and no option that names a program;
//     In a task worktree only through GitWorkspaceDirs, which names the
//     worktree's git dir (S6-D3);
//   - GitHTTPBackend: exactly `git http-backend`, its CGI environment from an
//     allowlist;
//
// Both git kinds read no system config and only the daemon-composed global
// config (S6-D4, gitconfig.go).
//   - ConfiguredProgram: the program of a ConfiguredCommand, a value only the
//     config loader's hand-off mints (pinned per symbol by the architecture
//     law TestSpawnLaw_OnlyNamedPackagesImportSpawn).
//
// Each kind takes the argv its caller already builds and parses it against its
// grammar; anything outside it is refused with ErrRefused before a process
// exists. No kind runs a shell.
package spawn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrRefused marks an argv outside its kind's grammar. Nothing was started.
var ErrRefused = errors.New("spawn: refused by the process-spawn law")

// ErrNotFound is exec.ErrNotFound, so callers can recognise a missing program
// without importing os/exec.
var ErrNotFound = exec.ErrNotFound

// ExitError is exec.ExitError, for errors.As without importing os/exec.
type ExitError = exec.ExitError

// Kind names one of the closed set of programs.
type Kind string

// The kinds (design §3).
const (
	KindPodmanAgent       Kind = "PodmanAgent"
	KindPodmanControl     Kind = "PodmanControl"
	KindGitWorkspace      Kind = "GitWorkspace"
	KindGitHTTPBackend    Kind = "GitHTTPBackend"
	KindConfiguredProgram Kind = "ConfiguredProgram"
)

func refuse(kind Kind, format string, a ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrRefused, kind, fmt.Sprintf(format, a...))
}

// Cmd is a prepared process of one kind. It exposes what the daemon's callers
// need and nothing that changes the program or its argv.
type Cmd struct {
	c    *exec.Cmd
	kind Kind
	// gitSub is the GitWorkspace subcommand, for WithGitHTTPAuthHeader.
	gitSub string
}

func newCmd(ctx context.Context, kind Kind, program string, args []string) *Cmd {
	return &Cmd{c: exec.CommandContext(ctx, program, args...), kind: kind}
}

// Kind is the kind the command was accepted as.
func (c *Cmd) Kind() Kind { return c.kind }

// Args is the full argv, program first (a copy).
func (c *Cmd) Args() []string { return append([]string(nil), c.c.Args...) }

// Env is the environment set on the command, nil when it inherits the daemon's.
func (c *Cmd) Env() []string { return append([]string(nil), c.c.Env...) }

// Dir is the working directory, "" for the daemon's.
func (c *Cmd) Dir() string { return c.c.Dir }

// SetStdin sets the process's standard input.
func (c *Cmd) SetStdin(r io.Reader) { c.c.Stdin = r }

// SetStdout sets the process's standard output.
func (c *Cmd) SetStdout(w io.Writer) { c.c.Stdout = w }

// SetStderr sets the process's standard error.
func (c *Cmd) SetStderr(w io.Writer) { c.c.Stderr = w }

// Output runs the command and returns its standard output.
func (c *Cmd) Output() ([]byte, error) { return c.c.Output() }

// CombinedOutput runs the command and returns stdout and stderr together.
func (c *Cmd) CombinedOutput() ([]byte, error) { return c.c.CombinedOutput() }

// Run runs the command and waits for it.
func (c *Cmd) Run() error { return c.c.Run() }

// Start starts the command without waiting.
func (c *Cmd) Start() error { return c.c.Start() }

// Wait waits for a started command.
func (c *Cmd) Wait() error { return c.c.Wait() }

// StdinPipe returns a pipe connected to the command's standard input.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) { return c.c.StdinPipe() }

// StdoutPipe returns a pipe connected to the command's standard output.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) { return c.c.StdoutPipe() }

// Pid is the started process's id, 0 before Start.
func (c *Cmd) Pid() int {
	if c.c.Process == nil {
		return 0
	}
	return c.c.Process.Pid
}

// Started reports whether the process was started.
func (c *Cmd) Started() bool { return c.c.Process != nil }

// Kill kills the started process (not its group).
func (c *Cmd) Kill() error {
	if c.c.Process == nil {
		return errors.New("spawn: kill: process not started")
	}
	return c.c.Process.Kill()
}

// --- podman ---

// LookPodman resolves podman on PATH.
func LookPodman() (string, error) { return exec.LookPath("podman") }

// podmanProgram checks the configured podman path: the program is podman, at
// whatever path (a custom install, a test's fake), and never another binary.
func podmanProgram(kind Kind, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "podman", nil
	}
	if filepath.Base(path) != "podman" {
		return "", refuse(kind, "program %q is not podman", path)
	}
	return path, nil
}

// pinnedRepositories are the agent image's one name, pulled or built locally
// (packaged-image-provenance design; imagemanifest.AgentImageTag). Any tag or
// digest of it is the pinned image.
var pinnedRepositories = map[string]bool{
	"ghcr.io/grinco/vornik-agent": true,
	"localhost/vornik-agent":      true,
}

// IsPinnedAgentImage reports whether ref names the pinned agent image.
func IsPinnedAgentImage(ref string) bool {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if slash, colon := strings.LastIndex(ref, "/"), strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return pinnedRepositories[ref]
}

// agentFlag describes one `podman run` flag of the closed grammar: whether it
// takes a value and, when the value is pinned, the values allowed.
type agentFlag struct {
	value   bool
	allowed map[string]bool   // nil: any value
	valid   func(string) bool // when set, the value must satisfy it
}

// fsizeOnly accepts `fsize=<bytes>`: the sandbox runner's per-file output cap
// (process-spawn law S5b residual R1). Other ulimits could RAISE a limit.
func fsizeOnly(v string) bool {
	n, ok := strings.CutPrefix(v, "fsize=")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func set(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// agentFlags are exactly the flags the runtime and the sandbox runner emit.
// Anything else — --privileged, --cap-add, --device, --pid, --ipc, a
// --security-opt that weakens confinement — is refused.
var agentFlags = map[string]agentFlag{
	"--detach": {}, "-d": {}, "--replace": {}, "--rm": {}, "-i": {}, "--interactive": {}, "--read-only": {},
	"--name": {value: true}, "--label": {value: true}, "--env": {value: true}, "-e": {value: true},
	"--cpu-quota": {value: true}, "--cpus": {value: true}, "--memory": {value: true},
	"--memory-swap": {value: true}, "--pids-limit": {value: true}, "--volume": {value: true},
	"-v": {value: true}, "--workdir": {value: true}, "-w": {value: true}, "--tmpfs": {value: true},
	"--entrypoint": {value: true}, "--user": {value: true}, "--cap-drop": {value: true},
	"--network":      {value: true, allowed: set("none", "host")},
	"--userns":       {value: true, allowed: set("host", "keep-id", "private")},
	"--security-opt": {value: true, allowed: set("no-new-privileges", "label=disable")},
	"--pull":         {value: true, allowed: set("never")},
	"--ulimit":       {value: true, valid: fsizeOnly},
}

// parseFlags walks flags until the first non-flag token, checking each against
// grammar. It returns the index of that token (len(argv) when none).
func parseFlags(kind Kind, argv []string, grammar map[string]agentFlag) (int, error) {
	i := 0
	for i < len(argv) {
		tok := argv[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			return i, nil
		}
		if tok == "--" {
			return 0, refuse(kind, `"--" is not accepted`)
		}
		name, val, hasEq := strings.Cut(tok, "=")
		f, ok := grammar[name]
		if !ok {
			return 0, refuse(kind, "flag %q is not in the grammar", name)
		}
		if !f.value {
			if hasEq {
				return 0, refuse(kind, "flag %q takes no value", name)
			}
			i++
			continue
		}
		if !hasEq {
			if i+1 >= len(argv) {
				return 0, refuse(kind, "flag %q needs a value", name)
			}
			val = argv[i+1]
			i++
		}
		if f.allowed != nil && !f.allowed[strings.ToLower(strings.TrimSpace(val))] {
			return 0, refuse(kind, "%s=%q is not an allowed value", name, val)
		}
		if f.valid != nil && !f.valid(val) {
			return 0, refuse(kind, "%s=%q is not an allowed value", name, val)
		}
		i++
	}
	return i, nil
}

// PodmanAgent accepts `podman run [flags] <pinned image> [command...]`.
func PodmanAgent(ctx context.Context, podmanPath string, argv []string) (*Cmd, error) {
	prog, err := podmanProgram(KindPodmanAgent, podmanPath)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 || argv[0] != "run" {
		return nil, refuse(KindPodmanAgent, "only `podman run` (got %q)", strings.Join(argv, " "))
	}
	at, err := parseFlags(KindPodmanAgent, argv[1:], agentFlags)
	if err != nil {
		return nil, err
	}
	rest := argv[1+at:]
	if len(rest) == 0 {
		return nil, refuse(KindPodmanAgent, "no image")
	}
	if !IsPinnedAgentImage(rest[0]) {
		return nil, refuse(KindPodmanAgent, "image %q is not the pinned agent image", rest[0])
	}
	return newCmd(ctx, KindPodmanAgent, prog, argv), nil
}

// controlVerb is one PodmanControl verb: its flags and whether it takes
// exactly one target (a container, or for image inspect the pinned image).
type controlVerb struct {
	flags  map[string]agentFlag
	target bool
}

var controlVerbs = map[string]controlVerb{
	"version": {flags: map[string]agentFlag{"--format": {value: true}}},
	"stop":    {flags: map[string]agentFlag{"--time": {value: true}, "-t": {value: true}, "--ignore": {}, "-i": {}}, target: true},
	"rm":      {flags: map[string]agentFlag{"--force": {}, "-f": {}, "--ignore": {}, "-i": {}}, target: true},
	"inspect": {flags: map[string]agentFlag{"--format": {value: true}, "-f": {value: true}}, target: true},
	"ps": {flags: map[string]agentFlag{"--all": {}, "-a": {}, "--no-trunc": {}, "--quiet": {}, "-q": {},
		"--filter": {value: true}, "-f": {value: true}, "--format": {value: true}}},
	"wait": {target: true},
	"logs": {flags: map[string]agentFlag{"--tail": {value: true}}, target: true},
}

// PodmanControl accepts the daemon's podman verbs on its own containers and
// the pinned image. The target's provenance is not checked here: container ids
// come from the daemon's own podman output and labels (design, "not covered").
func PodmanControl(ctx context.Context, podmanPath string, argv []string) (*Cmd, error) {
	prog, err := podmanProgram(KindPodmanControl, podmanPath)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, refuse(KindPodmanControl, "empty argv")
	}
	switch argv[0] {
	case "system":
		if len(argv) != 2 || argv[1] != "migrate" {
			return nil, refuse(KindPodmanControl, "only `system migrate`")
		}
		return newCmd(ctx, KindPodmanControl, prog, argv), nil
	case "image":
		if len(argv) < 2 || argv[1] != "inspect" {
			return nil, refuse(KindPodmanControl, "only `image inspect`")
		}
		at, err := parseFlags(KindPodmanControl, argv[2:], controlVerbs["inspect"].flags)
		if err != nil {
			return nil, err
		}
		rest := argv[2+at:]
		if len(rest) != 1 || !IsPinnedAgentImage(rest[0]) {
			return nil, refuse(KindPodmanControl, "image inspect only of the pinned agent image")
		}
		return newCmd(ctx, KindPodmanControl, prog, argv), nil
	}
	verb, ok := controlVerbs[argv[0]]
	if !ok {
		return nil, refuse(KindPodmanControl, "verb %q is not a daemon podman verb", argv[0])
	}
	at, err := parseFlags(KindPodmanControl, argv[1:], verb.flags)
	if err != nil {
		return nil, err
	}
	rest := argv[1+at:]
	switch {
	case verb.target && len(rest) != 1:
		return nil, refuse(KindPodmanControl, "`%s` takes exactly one target", argv[0])
	case !verb.target && len(rest) != 0:
		return nil, refuse(KindPodmanControl, "`%s` takes no target", argv[0])
	case verb.target && (strings.TrimSpace(rest[0]) == "" || strings.ContainsAny(rest[0], " \t\n")):
		return nil, refuse(KindPodmanControl, "target %q", rest[0])
	}
	return newCmd(ctx, KindPodmanControl, prog, argv), nil
}

// --- git ---

var (
	rootsMu sync.RWMutex
	roots   []string
)

// RegisterWorkspaceRoot adds a directory under which GitWorkspace and
// GitHTTPBackend may run. The daemon wiring registers
// runtime.project_workspace_path; with nothing registered both kinds refuse
// everything (fail closed). A relative root is taken against the working
// directory, as git -C would take it; "/" is refused.
func RegisterWorkspaceRoot(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("spawn: empty workspace root")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("spawn: workspace root %q: %w", dir, err)
	}
	dir = abs
	if dir == string(filepath.Separator) {
		return errors.New("spawn: / is not a workspace root")
	}
	rootsMu.Lock()
	defer rootsMu.Unlock()
	for _, r := range roots {
		if r == dir {
			return nil
		}
	}
	roots = append(roots, dir)
	return nil
}

// resolved follows symlinks when the path exists, so a link inside a root
// that points out of it is judged by where it points.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// underRoot reports whether dir is a registered root or lies beneath one. A
// relative dir is taken against the working directory, as git -C takes it.
func underRoot(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	d := resolved(abs)
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	for _, r := range roots {
		if within(d, resolved(r)) {
			return true
		}
	}
	return false
}

// gitSubcommands are the ones the orchestration runs.
var gitSubcommands = set("add", "branch", "cat-file", "checkout", "clean", "commit", "config",
	"diff", "fetch", "format-patch", "init", "log", "ls-files", "merge", "push", "remote",
	"reset", "rev-list", "rev-parse", "rm", "status", "update-ref", "worktree")

// gitProgramOptions name a program git would run; refused everywhere.
var gitProgramOptions = []string{"--upload-pack", "--receive-pack", "--exec", "--ext-diff", "--textconv", "--config-env"}

// GitWorkspace accepts `git -C <dir> [-c user.name=… | -c user.email=…]…
// <subcommand> args…` with dir under a registered workspace root. A dir inside
// a task worktree is refused: those commands go through GitWorkspaceDirs,
// which names the worktree's git dir (S6-D3).
func GitWorkspace(ctx context.Context, dir string, args ...string) (*Cmd, error) {
	return gitWorkspace(ctx, dir, GitDirs{}, args)
}

func gitWorkspace(ctx context.Context, dir string, dirs GitDirs, args []string) (*Cmd, error) {
	if !underRoot(dir) {
		return nil, refuse(KindGitWorkspace, "directory %q is not under a registered workspace root", dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, refuse(KindGitWorkspace, "directory %q: %v", dir, err)
	}
	if err := checkGitDirs(resolved(abs), dirs); err != nil {
		return nil, err
	}
	i := 0
	for i < len(args) && args[i] == "-c" {
		if i+1 >= len(args) {
			return nil, refuse(KindGitWorkspace, "-c needs a value")
		}
		key, _, _ := strings.Cut(args[i+1], "=")
		if key != "user.name" && key != "user.email" {
			return nil, refuse(KindGitWorkspace, "-c %s: only user.name and user.email", key)
		}
		i += 2
	}
	if i >= len(args) {
		return nil, refuse(KindGitWorkspace, "no subcommand")
	}
	sub := args[i]
	if !gitSubcommands[sub] {
		return nil, refuse(KindGitWorkspace, "subcommand %q is not in the orchestration set", sub)
	}
	rest := args[i+1:]
	switch sub {
	case "config":
		if len(rest) != 2 || rest[0] != "receive.denyCurrentBranch" {
			return nil, refuse(KindGitWorkspace, "config may set only receive.denyCurrentBranch")
		}
	case "remote":
		if len(rest) < 1 || rest[0] != "get-url" {
			return nil, refuse(KindGitWorkspace, "remote may only get-url")
		}
	}
	for _, a := range rest {
		for _, o := range gitProgramOptions {
			if a == o || strings.HasPrefix(a, o+"=") {
				return nil, refuse(KindGitWorkspace, "option %q names a program", o)
			}
		}
	}
	c := newCmd(ctx, KindGitWorkspace, "git", append([]string{"-C", dir}, args...))
	c.gitSub = sub
	c.c.Env = daemonGitEnv(dirs)
	return c, nil
}

// WithGitHTTPAuthHeader passes an HTTP header to git's transport through
// GIT_CONFIG_* (http.extraheader), never argv or disk, and disables the
// credential prompt. Only for a GitWorkspace push or fetch.
func (c *Cmd) WithGitHTTPAuthHeader(header string) error {
	if c.kind != KindGitWorkspace || (c.gitSub != "push" && c.gitSub != "fetch") {
		return refuse(c.kind, "an HTTP auth header is only for a git push or fetch")
	}
	c.c.Env = append(c.c.Env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0="+header,
		"GIT_TERMINAL_PROMPT=0",
	)
	return nil
}

// gitCGIEnv is the CGI environment git http-backend may receive.
var gitCGIEnv = set("GIT_PROJECT_ROOT", "GIT_HTTP_EXPORT_ALL", "PATH_INFO", "REQUEST_METHOD",
	"QUERY_STRING", "REMOTE_USER", "CONTENT_TYPE", "CONTENT_LENGTH", "GIT_PROTOCOL", "PATH")

// GitHTTPBackend accepts exactly `git http-backend` with env from the CGI
// allowlist and GIT_PROJECT_ROOT under a registered workspace root.
func GitHTTPBackend(ctx context.Context, env []string) (*Cmd, error) {
	root := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if !gitCGIEnv[k] {
			return nil, refuse(KindGitHTTPBackend, "environment variable %q is not in the CGI allowlist", k)
		}
		if k == "GIT_PROJECT_ROOT" {
			root = v
		}
	}
	if !underRoot(root) {
		return nil, refuse(KindGitHTTPBackend, "GIT_PROJECT_ROOT %q is not under a registered workspace root", root)
	}
	c := newCmd(ctx, KindGitHTTPBackend, "git", []string{"http-backend"})
	c.c.Env = append(append([]string(nil), env...), gitConfigPins()...)
	return c, nil
}

// --- configured programs ---

// ConfiguredCommand is a program named in the operator's config files: a stdio
// MCP server or a CLI chat provider. Its fields are unexported, so a request
// handler cannot fill one; which packages may call NewConfiguredCommand is
// pinned by the architecture law. The zero value is refused.
type ConfiguredCommand struct {
	path string
	args []string
}

// NewConfiguredCommand mints a ConfiguredCommand from loaded config. Only the
// config loader's hand-off may call it (TestSpawnLaw_OnlyNamedPackagesImportSpawn).
// An empty program mints the zero value.
func NewConfiguredCommand(program string, args ...string) ConfiguredCommand {
	if strings.TrimSpace(program) == "" {
		return ConfiguredCommand{}
	}
	return ConfiguredCommand{path: program, args: append([]string(nil), args...)}
}

// IsZero reports whether no program was configured.
func (c ConfiguredCommand) IsZero() bool { return c.path == "" }

// Path is the configured program.
func (c ConfiguredCommand) Path() string { return c.path }

// Args are the configured base arguments (a copy).
func (c ConfiguredCommand) Args() []string { return append([]string(nil), c.args...) }

// ConfiguredOptions are what a caller may vary per launch. The program is not
// among them.
type ConfiguredOptions struct {
	// ArgMapper, when set, rewrites each CONFIGURED argument (the MCP client's
	// restricted ${VAR} expansion). Per-call Args are passed as given.
	ArgMapper func(string) string
	// Args are appended after the configured arguments.
	Args []string
	// Env replaces the environment when non-nil.
	Env []string
	// Dir is the working directory ("" inherits the daemon's).
	Dir string
	// ProcessGroup starts the process in its own group, so a group kill
	// reaches what it forks.
	ProcessGroup bool
	// WaitDelay bounds Wait after the process exits (exec.Cmd.WaitDelay).
	WaitDelay time.Duration
}

// ConfiguredProgram prepares the configured program. A caller that must not
// tie the process to a request's lifetime passes context.Background().
func ConfiguredProgram(ctx context.Context, cc ConfiguredCommand, opts ConfiguredOptions) (*Cmd, error) {
	if cc.IsZero() {
		return nil, refuse(KindConfiguredProgram, "no program configured (a ConfiguredCommand comes only from the config files)")
	}
	args := make([]string, 0, len(cc.args)+len(opts.Args))
	for _, a := range cc.args {
		if opts.ArgMapper != nil {
			a = opts.ArgMapper(a)
		}
		args = append(args, a)
	}
	args = append(args, opts.Args...)
	c := newCmd(ctx, KindConfiguredProgram, cc.path, args)
	if opts.Env != nil {
		c.c.Env = append([]string(nil), opts.Env...)
	}
	c.c.Dir = opts.Dir
	if opts.ProcessGroup {
		c.c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	c.c.WaitDelay = opts.WaitDelay
	return c, nil
}
