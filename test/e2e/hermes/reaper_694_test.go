package hermes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"vornik.io/vornik/internal/testutil/testenv"
)

// GitHub #694 (lane design, amendment 2026-11-11). Found at e2e-lane: a lane
// run hit go test's timeout on the CI VM; its daemon and containers outlived
// the binary and held the run script's flock (fd 9), so the next run waited
// six hours. Lower because: a lane timeout takes an hour to reproduce; both
// mechanisms run here on real processes.

func TestMarkInheritedCloseOnExec_694(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(p, syscall.O_RDONLY, 0) // no O_CLOEXEC: as a shell's 9>lock
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	cloexec := func() bool {
		flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if e != 0 {
			t.Fatal(e)
		}
		return flags&syscall.FD_CLOEXEC != 0
	}
	if cloexec() {
		t.Fatal("precondition: the descriptor is already close-on-exec")
	}
	markInheritedCloseOnExec()
	if !cloexec() {
		t.Fatal("an inherited descriptor is still passed to every child")
	}
}

func TestReaper_694(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	podmanLog := filepath.Join(dir, "podman.log")
	stub := "#!/bin/sh\necho \"$*\" >> " + podmanLog + "\n[ \"$1\" = ps ] && echo c1 c2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	start := func(script string) *exec.Cmd {
		t.Helper()
		c := exec.Command("bash", "-c", script)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		// Kill only: each process is waited on exactly once, by waited or by
		// the test itself (two Waits on one Cmd race).
		t.Cleanup(func() { _ = c.Process.Kill() })
		return c
	}
	waited := func(c *exec.Cmd) chan struct{} {
		ch := make(chan struct{})
		go func() { _ = c.Wait(); close(ch) }()
		return ch
	}
	parent := start("sleep 60")
	daemon := start("sleep 60")
	stubborn := start(`trap "" TERM; sleep 20 & wait; sleep 20`)
	stranger := start("sleep 60")
	daemonDone, stubbornDone, strangerDone := waited(daemon), waited(stubborn), waited(stranger)
	time.Sleep(200 * time.Millisecond) // let bash install its trap
	pids := filepath.Join(dir, "run.pids")
	lines := fmt.Sprintf("%d %s\n%d %s\n%d 1\n", daemon.Process.Pid, procStartTime(daemon.Process.Pid),
		stubborn.Process.Pid, procStartTime(stubborn.Process.Pid), stranger.Process.Pid) // the stranger: a recycled PID
	if err := os.WriteFile(pids, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Create(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	r := exec.Command("bash", "-c", reaperScript(parent.Process.Pid, procStartTime(parent.Process.Pid), "abc123", pids, 1, 2))
	r.Env = testenv.Clean("PATH=" + bin + ":" + os.Getenv("PATH"))
	r.ExtraFiles = []*os.File{lock} // fd 3 in the reaper: the inherited lock
	r.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	reaperDone := waited(r)
	t.Cleanup(func() { _ = r.Process.Kill() })

	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Lstat(fmt.Sprintf("/proc/%d/fd/3", r.Process.Pid)); err == nil {
		t.Error("the reaper still holds the descriptor it inherited")
	}
	select {
	case <-daemonDone:
		t.Fatal("the daemon was signalled while the test binary lived")
	default:
	}
	if _, err := os.Stat(podmanLog); err == nil {
		t.Fatal("podman was called while the test binary lived")
	}

	_ = parent.Process.Kill()
	_ = parent.Wait() // reap it, so its PID is gone
	for name, ch := range map[string]chan struct{}{"daemon": daemonDone, "SIGTERM-ignoring daemon": stubbornDone, "reaper": reaperDone} {
		select {
		case <-ch:
		case <-time.After(15 * time.Second):
			t.Fatalf("the %s did not exit after the test binary was gone", name)
		}
	}
	select {
	case <-strangerDone:
		t.Fatal("a process whose start time did not match (a recycled PID) was killed")
	default:
	}
	log, _ := os.ReadFile(podmanLog)
	if !strings.Contains(string(log), "ps -aq --filter label=vornik-e2e-run=abc123") || !strings.Contains(string(log), "rm -f c1 c2") {
		t.Fatalf("podman calls:\n%s", log)
	}
	if _, err := os.Stat(pids); err == nil {
		t.Fatal("the pid file was left behind")
	}
}

func TestRecordAndForgetDaemon_694(t *testing.T) {
	prev := reaperPIDs
	reaperPIDs = filepath.Join(t.TempDir(), "x.pids")
	t.Cleanup(func() { reaperPIDs = prev })
	if err := recordDaemon(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := recordDaemon(1); err != nil {
		t.Fatal(err)
	}
	forgetDaemon(os.Getpid())
	b, _ := os.ReadFile(reaperPIDs)
	if got := strings.TrimSpace(string(b)); !strings.HasPrefix(got, "1 ") || strings.Contains(got, "\n") {
		t.Fatalf("pid file = %q, want only pid 1's line", got)
	}
	if procStartTime(os.Getpid()) == "" || procStartTime(1<<30) != "" {
		t.Fatal("procStartTime: want a value for this process and none for an absent PID")
	}
}

// Code review 1909 F1: a parent whose start time cannot be read would make
// the reaper spin forever; it exits instead.
func TestReaper_694_ExitsWithoutAParentStartTime(t *testing.T) {
	r := exec.Command("bash", "-c", reaperScript(os.Getpid(), "", "abc", filepath.Join(t.TempDir(), "p"), 1, 1))
	done := make(chan struct{})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = r.Process.Kill()
		t.Fatal("the reaper kept running with no parent start time")
	}
}
