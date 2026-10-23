package spawn

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// GitHub #369 item 3 (2026-10-17, outcome-quality-design §S8.8, §S8.3 #3):
// found at review (the §S8 census). A ConfiguredProgram started in its own
// process group was cancelled with exec's default Cancel, Process.Kill, which
// reaches the leader only: whatever it forked lived on. With ProcessGroup set,
// the context's end now kills the group.

// groupFixture starts sh, which forks a sleep and records its pid, under ctx.
func groupFixture(ctx context.Context, t *testing.T, opts ConfiguredOptions) (c *Cmd, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	cc := NewConfiguredCommand("/bin/sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
	c, err := ConfiguredProgram(ctx, cc, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fixture never recorded its child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		if pid := readPid(pidFile); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return c, pidFile
}

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// running reports whether pid is alive and not a zombie.
func running(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for running(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func TestConfiguredProgram_ProcessGroupCancelKillsTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, pidFile := groupFixture(ctx, t, ConfiguredOptions{ProcessGroup: true, WaitDelay: 200 * time.Millisecond})
	cancel()
	_ = c.Wait()
	if !waitGone(readPid(pidFile), 3*time.Second) {
		t.Fatalf("the forked child %d outlived the cancelled leader: the cancel reached the leader only (GitHub #369)",
			readPid(pidFile))
	}
}

// Without ProcessGroup nothing changes: the default cancel kills the leader
// only, and the child is not in a group of its own to kill.
func TestConfiguredProgram_NoProcessGroupKeepsTheDefaultCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, pidFile := groupFixture(ctx, t, ConfiguredOptions{WaitDelay: 200 * time.Millisecond})
	cancel()
	_ = c.Wait()
	if !running(readPid(pidFile)) {
		t.Fatal("without ProcessGroup the child is expected to survive the leader's kill (as built)")
	}
}

// A descendant that left the group (setsid) is out of the group kill's reach,
// as the design says; WaitDelay is what still lets Wait return when it holds
// the leader's stdout.
func TestConfiguredProgram_ASetsidDescendantStillLetsWaitReturn(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	cc := NewConfiguredCommand("/bin/sh", "-c", "/usr/bin/setsid sh -c 'echo $$ > "+pidFile+"; exec sleep 300' & wait")
	c, err := ConfiguredProgram(ctx, cc, ConfiguredOptions{ProcessGroup: true, WaitDelay: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.SetStdout(&strings.Builder{}) // a copying goroutine, so WaitDelay closes the pipe
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pid := readPid(pidFile); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for readPid(pidFile) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the fixture never recorded its setsid child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return: WaitDelay no longer backs up the group kill")
	}
	if !running(readPid(pidFile)) {
		t.Fatal("the setsid child was expected to escape the group kill")
	}
}

func TestCmd_KillGroupKillsTheGroupAndRefusesDegeneratePids(t *testing.T) {
	c, pidFile := groupFixture(context.Background(), t, ConfiguredOptions{ProcessGroup: true})
	if err := c.KillGroup(); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	if !waitGone(readPid(pidFile), 3*time.Second) {
		t.Fatal("KillGroup left the forked child running")
	}
	for _, pid := range []int{0, -1} {
		if err := killGroup(pid, func() error {
			t.Fatalf("pid %d must be refused, not signalled", pid)
			return nil
		}); err != nil {
			t.Fatalf("pid %d: %v", pid, err)
		}
	}
	if err := (&Cmd{}).KillGroup(); err == nil {
		t.Fatal("KillGroup on a command never started must error")
	}
}
