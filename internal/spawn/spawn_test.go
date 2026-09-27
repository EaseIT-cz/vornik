package spawn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the process-spawn law's kinds
// (https://docs.vornik.io §3, "S1b-2, as
// built"): each kind refuses an argv outside its closed grammar BEFORE any
// process exists. A refusal is errors.Is(err, ErrRefused) and returns no Cmd.

func refused(t *testing.T, c *Cmd, err error, why string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrRefused) {
		t.Fatalf("%s: want ErrRefused, got cmd=%v err=%v", why, c, err)
	}
	if c != nil {
		t.Fatalf("%s: a refusal must return no Cmd", why)
	}
}

func accepted(t *testing.T, c *Cmd, err error, why string) *Cmd {
	t.Helper()
	if err != nil || c == nil {
		t.Fatalf("%s: want accepted, got err=%v", why, err)
	}
	return c
}

const pinned = "ghcr.io/grinco/vornik-agent:latest"

func TestPodmanAgent_AcceptsTheShapesTheDaemonBuilds(t *testing.T) {
	ctx := context.Background()
	// The runtime's agent start (buildPreImageArgs + userns + image).
	runtimeArgv := []string{"run", "--detach", "--replace", "--name", "vornik-p-r-t",
		"--label", "io.vornik.managed=true", "--env", "A=B", "--cpu-quota", "5000",
		"--memory", "1073741824", "--volume", "/in:/app/input:ro,Z",
		"--workdir", "/app/workspace", "--security-opt", "no-new-privileges",
		"--cap-drop", "ALL", "--user", "1000:1000", "--network", "none",
		"--security-opt", "label=disable", "--volume", "/run/v.sock:/run/vornik.sock:ro",
		"--userns", "keep-id", pinned}
	c := must(t, "runtime start")(PodmanAgent(ctx, "", runtimeArgv))
	if got := c.Args(); got[0] != "podman" || got[len(got)-1] != pinned {
		t.Fatalf("argv = %v", got)
	}
	// The sandbox one-shot (sandboxtool.argv), with a command after the image.
	sandboxArgv := []string{"run", "--name", "vornik-sbx-ab", "--label", "io.vornik.sandbox-run=x",
		"--network=none", "--pull=never", "--userns=keep-id", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,size=256m",
		"--memory=5", "--memory-swap=5", "--pids-limit=64", "--cpus=1", "-e", "HOME=/tmp",
		"--ulimit", "fsize=268435456",
		"-v", "/a:/in:ro,Z", "-v", "/b:/out:Z", "-i", "-w", "/out", "--entrypoint", "pdftotext",
		"localhost/vornik-agent:e2e", "-q", "/in/document.pdf", "/out/text.txt"}
	must(t, "sandbox one-shot")(PodmanAgent(ctx, "/usr/bin/podman", sandboxArgv))
	// A digest-pinned reference is the same image.
	must(t, "digest")(PodmanAgent(ctx, "", []string{"run", "ghcr.io/grinco/vornik-agent@sha256:abc"}))
}

func TestPodmanAgent_RefusesAnImageThatIsNotThePinnedAgentImage(t *testing.T) {
	ctx := context.Background()
	for _, img := range []string{
		"alpine:latest",
		"docker.io/attacker/vornik-agent:latest", // right name, wrong registry
		"vornik-agent:latest",                    // bare: the runtime qualifies it first
		"ghcr.io/grinco/vornik-agent-evil:1",
		"localhost/other:latest",
	} {
		c, err := PodmanAgent(ctx, "", []string{"run", "--detach", img})
		refused(t, c, err, "image "+img)
	}
	c, err := PodmanAgent(ctx, "", []string{"run", "--detach"})
	refused(t, c, err, "no image")
}

func TestPodmanAgent_RefusesFlagsOutsideTheGrammar(t *testing.T) {
	ctx := context.Background()
	for _, flags := range [][]string{
		{"--privileged"},
		{"--ulimit", "nofile=1048576"}, // only the fsize cap; another ulimit could raise a limit
		{"--ulimit", "fsize=unlimited"},
		{"--ulimit=fsize=-1"},
		{"--cap-add", "SYS_ADMIN"},
		{"--cap-add=ALL"},
		{"--device", "/dev/kvm"},
		{"--pid", "host"},
		{"--ipc=host"},
		{"--network", "container:x"},
		{"--userns", "ns:/proc/1/ns/user"},
		{"--security-opt", "seccomp=unconfined"},
		{"--pull", "always"},
		{"--pull=missing"},
		{"--name"}, // a value flag with no value swallows the image
	} {
		argv := append(append([]string{"run"}, flags...), pinned)
		c, err := PodmanAgent(ctx, "", argv)
		refused(t, c, err, strings.Join(flags, " "))
	}
	c, err := PodmanAgent(ctx, "", []string{"create", pinned})
	refused(t, c, err, "not run")
}

func TestPodman_RefusesAProgramThatIsNotPodman(t *testing.T) {
	ctx := context.Background()
	c, err := PodmanAgent(ctx, "/bin/sh", []string{"run", pinned})
	refused(t, c, err, "agent via /bin/sh")
	c, err = PodmanControl(ctx, "/tmp/evil", []string{"ps"})
	refused(t, c, err, "control via /tmp/evil")
	c, err = PodmanControl(ctx, "/opt/fake/podman", []string{"ps", "-a"})
	accepted(t, c, err, "a podman at another path (tests, custom installs)")
}

func TestPodmanControl_AcceptsTheDaemonsVerbs(t *testing.T) {
	ctx := context.Background()
	for _, argv := range [][]string{
		{"version", "--format", "json"},
		{"system", "migrate"},
		{"stop", "--time", "10", "abc123"},
		{"rm", "--force", "abc123"},
		{"rm", "-f", "--ignore", "vornik-sbx-1"},
		{"inspect", "--format", "json", "abc123"},
		{"inspect", "--format", "{{.State.OOMKilled}}", "vornik-sbx-1"},
		{"ps", "--all", "--format", "json", "--filter", "label=a=b"},
		{"ps", "-a", "--filter", "label=x=y", "--format", "{{.Names}}"},
		{"ps", "--no-trunc", "--filter", "status=running", "--format", "{{.Mounts}}"},
		{"wait", "abc123"},
		{"logs", "--tail", "5", "abc123"},
		{"logs", "abc123"},
		{"image", "inspect", "--format", `{{index .Labels "io.vornik.sandbox-tools"}}`, pinned},
	} {
		c, err := PodmanControl(ctx, "", argv)
		accepted(t, c, err, strings.Join(argv, " "))
	}
}

func TestPodmanControl_RefusesEverythingElse(t *testing.T) {
	ctx := context.Background()
	for _, argv := range [][]string{
		{"run", pinned},                   // run is PodmanAgent's
		{"exec", "abc", "sh"},             // not a daemon verb
		{"pull", pinned},                  // pulls are outside the closed set
		{"system", "reset"},               // only migrate
		{"stop", "a", "b"},                // exactly one target
		{"rm"},                            // no target
		{"rm", "--volumes", "abc"},        // flag not in rm's set
		{"logs", "--follow", "abc"},       // not in logs' set
		{"ps", "abc"},                     // ps takes no target
		{"inspect", "-f", "x", "--", "a"}, // "--" is not a target
		{"image", "inspect", "alpine"},    // image inspect only of the pinned image
		{"image", "rm", pinned},
		{"cp", "a:/etc/passwd", "/tmp"},
		{},
	} {
		c, err := PodmanControl(ctx, "", argv)
		refused(t, c, err, strings.Join(argv, " "))
	}
}

func withRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	resetRootsForTest(t)
	if err := RegisterWorkspaceRoot(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGitWorkspace_RefusesADirectoryOutsideTheRegisteredRoot(t *testing.T) {
	ctx := context.Background()
	root := withRoot(t)
	proj := filepath.Join(root, "p1")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	c := must(t, "inside the root")(GitWorkspace(ctx, proj, "rev-parse", "HEAD"))
	if got := strings.Join(c.Args(), " "); got != "git -C "+proj+" rev-parse HEAD" {
		t.Fatalf("argv = %q", got)
	}

	outside := t.TempDir()
	c2, err := GitWorkspace(ctx, outside, "status")
	refused(t, c2, err, "a sibling temp dir")
	c2, err = GitWorkspace(ctx, "/", "status")
	refused(t, c2, err, "/")
	c2, err = GitWorkspace(ctx, "relative/dir", "status")
	refused(t, c2, err, "a relative dir (taken against the cwd, which is outside the root)")
	c2, err = GitWorkspace(ctx, filepath.Join(root, "..", filepath.Base(outside)), "status")
	refused(t, c2, err, "a .. escape")

	// A symlink inside the root that points out of it is outside it.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	c2, err = GitWorkspace(ctx, link, "status")
	refused(t, c2, err, "a symlink out of the root")
}

func TestGitWorkspace_NoRegisteredRootRefusesEverything(t *testing.T) {
	resetRootsForTest(t)
	c, err := GitWorkspace(context.Background(), t.TempDir(), "status")
	refused(t, c, err, "nothing registered (fail closed)")
	if err := RegisterWorkspaceRoot("/"); err == nil {
		t.Fatal("registering / as a workspace root must be refused")
	}
	if err := RegisterWorkspaceRoot(""); err == nil {
		t.Fatal("registering an empty root must be refused")
	}
}

func TestGitWorkspace_RefusesArgvOutsideItsShape(t *testing.T) {
	ctx := context.Background()
	root := withRoot(t)
	must(t, "identity -c")(GitWorkspace(ctx, root, "-c", "user.name=vornik-agent", "-c", "user.email=agent@vornik.io", "commit", "-m", "x"))
	must(t, "the guard's config key")(GitWorkspace(ctx, root, "config", "receive.denyCurrentBranch", "updateInstead"))
	must(t, "remote get-url")(GitWorkspace(ctx, root, "remote", "get-url", "origin"))
	for _, args := range [][]string{
		{"-c", "core.hooksPath=/tmp/h", "status"},
		{"-c", "core.fsmonitor=/tmp/x", "status"},
		{"-C", "/etc", "status"},
		{"--git-dir=/etc", "status"},
		{"--exec-path=/tmp", "status"},
		{"config", "core.hooksPath", "/tmp/h"},
		{"config", "core.sshCommand", "sh"},
		{"remote", "add", "x", "ext::sh -c id"},
		{"fetch", "--upload-pack=touch /tmp/x", "origin"},
		{"push", "--receive-pack", "sh", "origin", "x"},
		{"diff", "--ext-diff"},
		{"log", "--textconv"},
		{"http-backend"}, // its own kind
		{"daemon"},
		{"filter-branch"},
		{"submodule", "update"},
		{},
	} {
		c, err := GitWorkspace(ctx, root, args...)
		refused(t, c, err, strings.Join(args, " "))
	}
}

func TestGitWorkspace_HTTPAuthHeaderTravelsInEnvNotArgv(t *testing.T) {
	ctx := context.Background()
	root := withRoot(t)
	c := must(t, "push")(GitWorkspace(ctx, root, "push", "origin", "abc:refs/heads/b"))
	if err := c.WithGitHTTPAuthHeader("Authorization: Basic s3cret"); err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Args() {
		if strings.Contains(a, "s3cret") {
			t.Fatalf("token reached argv: %v", c.Args())
		}
	}
	env := strings.Join(c.Env(), "\n")
	if !strings.Contains(env, "GIT_CONFIG_VALUE_0=Authorization: Basic s3cret") || !strings.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("env missing the header: %s", env)
	}
	st := must(t, "status")(GitWorkspace(ctx, root, "status"))
	if err := st.WithGitHTTPAuthHeader("x"); !errors.Is(err, ErrRefused) {
		t.Fatalf("an auth header on a non-network subcommand must be refused, got %v", err)
	}
}

func TestGitHTTPBackend_PinsTheCGIEnvironment(t *testing.T) {
	ctx := context.Background()
	root := withRoot(t)
	good := []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "PATH_INFO=/p/info/refs",
		"REQUEST_METHOD=GET", "QUERY_STRING=service=git-upload-pack", "REMOTE_USER=u",
		"CONTENT_TYPE=x", "CONTENT_LENGTH=0", "GIT_PROTOCOL=version=2", "PATH=/usr/bin"}
	c := must(t, "the handler's env")(GitHTTPBackend(ctx, good))
	if got := strings.Join(c.Args(), " "); got != "git http-backend" {
		t.Fatalf("argv = %q", got)
	}
	for _, extra := range []string{"GIT_CONFIG_COUNT=1", "GIT_EXEC_PATH=/tmp", "GIT_DIR=/etc", "LD_PRELOAD=/tmp/x.so", "GIT_SSH_COMMAND=sh"} {
		c, err := GitHTTPBackend(ctx, append(append([]string{}, good...), extra))
		refused(t, c, err, extra)
	}
	c2, err := GitHTTPBackend(ctx, []string{"GIT_PROJECT_ROOT=" + t.TempDir(), "PATH_INFO=/x"})
	refused(t, c2, err, "a project root outside the registered root")
	c2, err = GitHTTPBackend(ctx, []string{"PATH_INFO=/x"})
	refused(t, c2, err, "no project root")
}

func TestConfiguredProgram_RefusesAValueTheLoaderDidNotMint(t *testing.T) {
	var zero ConfiguredCommand
	c, err := ConfiguredProgram(context.Background(), zero, ConfiguredOptions{})
	refused(t, c, err, "zero ConfiguredCommand")

	cc := NewConfiguredCommand("/usr/bin/env", "FOO=1")
	c = must(t, "a minted command")(ConfiguredProgram(context.Background(), cc, ConfiguredOptions{
		Args:      []string{"true"},
		ArgMapper: strings.ToUpper,
		Dir:       "/",
		Env:       []string{"A=B"},
	}))
	if got := strings.Join(c.Args(), " "); got != "/usr/bin/env FOO=1 true" {
		t.Fatalf("argv = %q (the mapper applies to configured args only)", got)
	}
	if c.Dir() != "/" || strings.Join(c.Env(), ",") != "A=B" {
		t.Fatalf("options not applied: dir=%q env=%v", c.Dir(), c.Env())
	}
	if cc.Path() != "/usr/bin/env" || len(cc.Args()) != 1 || cc.IsZero() {
		t.Fatalf("accessors: %q %v", cc.Path(), cc.Args())
	}
	if !NewConfiguredCommand("").IsZero() {
		t.Fatal("an empty program mints the zero value")
	}
}

func TestCmd_RunsAndReportsExitErrors(t *testing.T) {
	cc := NewConfiguredCommand("/bin/sh", "-c")
	c := must(t, "sh")(ConfiguredProgram(context.Background(), cc, ConfiguredOptions{Args: []string{"echo hi; exit 3"}}))
	out, err := c.CombinedOutput()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if c.Kind() != KindConfiguredProgram {
		t.Fatalf("kind = %s", c.Kind())
	}
	missing := NewConfiguredCommand("definitely-not-a-program-vornik")
	c2 := must(t, "missing")(ConfiguredProgram(context.Background(), missing, ConfiguredOptions{}))
	if _, err := c2.Output(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// must adapts accepted() so a (Cmd, error) call can be checked inline.
func must(t *testing.T, why string) func(*Cmd, error) *Cmd {
	t.Helper()
	return func(c *Cmd, err error) *Cmd { t.Helper(); return accepted(t, c, err, why) }
}
