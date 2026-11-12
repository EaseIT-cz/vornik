package hermes

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// A timed-out test binary runs no t.Cleanup, so its daemon and containers
// outlived it and held the run script's lock (GitHub #694; lane design,
// amendment 2026-11-11). Two defences, both here so they are unit-tested
// without podman: no child inherits a caller's descriptor, and a reaper
// outside the binary's process group cleans up once the binary is gone.

// markInheritedCloseOnExec marks every descriptor above 2 close-on-exec.
// TestMain calls it before m.Run, so a lock the caller left open (fd 9 of the
// VM's run script) reaches no process the lane starts.
func markInheritedCloseOnExec() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes lane: cannot list /proc/self/fd (%v): inherited descriptors stay open to children (GitHub #694)\n", err)
		return
	}
	for _, e := range entries {
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}

// procStartTime is field 22 of /proc/<pid>/stat (clock ticks since boot), or
// "" when the process is gone. A PID with a different start time is another
// process.
func procStartTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+1:] // comm may hold spaces; fields restart at 3 after ")"
	}
	f := strings.Fields(s)
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// reaperScript is the bash the reaper runs. Every poll seconds it checks that
// the parent still has its recorded start time; once it does not, it sends
// SIGTERM to each pid-file entry whose start time still matches, waits up to
// grace seconds, re-checks and sends SIGKILL, then removes the run's labelled
// containers and the pid file. It first closes every inherited descriptor
// above 2, so it can never hold a lock itself.
func reaperScript(parentPID int, parentStart, suffix, pidFile string, poll, grace int) string {
	return fmt.Sprintf(`
for fd in /proc/$$/fd/*; do n=${fd##*/}; case $n in 0|1|2) ;; *) eval "exec $n>&-" 2>/dev/null ;; esac; done
st() { local s; s=$(cat /proc/$1/stat 2>/dev/null) || return 0; s=${s##*) }; set -- $s; echo "${20}"; }
parent=%d; pstart=%q; pids=%q; suffix=%q
[ -n "$pstart" ] || exit 0
while [ "$(st $parent)" = "$pstart" ]; do sleep %d; done
signal() { [ -f "$pids" ] || return 0; while read -r pid start; do [ -n "$pid" ] && [ "$(st "$pid")" = "$start" ] && kill -"$1" "$pid" 2>/dev/null; done < "$pids"; return 0; }
alive() { [ -f "$pids" ] || return 1; while read -r pid start; do [ -n "$pid" ] && [ "$(st "$pid")" = "$start" ] && return 0; done < "$pids"; return 1; }
signal TERM
i=0; while [ $i -lt %d ] && alive; do sleep 1; i=$((i+1)); done
signal KILL
log="${pids%%.pids}.reaper.log"
ids=$(podman ps -aq --filter "label=%s=$suffix" 2>>"$log")
[ -n "$ids" ] && podman rm -f $ids >/dev/null 2>>"$log"
rm -f "$pids"
`, parentPID, parentStart, pidFile, suffix, poll, grace, runLabelKey)
}

// reaperPIDs is the run's pid file: daemons the reaper must stop.
var reaperPIDs = filepath.Join(os.TempDir(), "vornik-e2e-hermes-"+runSuffix+".pids")

var reaperPIDsMu sync.Mutex

// recordDaemon appends a daemon the reaper must stop: its PID and start time.
func recordDaemon(pid int) error {
	reaperPIDsMu.Lock()
	defer reaperPIDsMu.Unlock()
	f, err := os.OpenFile(reaperPIDs, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintf(f, "%d %s\n", pid, procStartTime(pid))
	return err
}

// forgetDaemon removes a daemon that has exited from the pid file.
func forgetDaemon(pid int) {
	reaperPIDsMu.Lock()
	defer reaperPIDsMu.Unlock()
	b, err := os.ReadFile(reaperPIDs)
	if err != nil {
		return
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" && !strings.HasPrefix(line, strconv.Itoa(pid)+" ") {
			keep = append(keep, line)
		}
	}
	out := strings.Join(keep, "\n")
	if out != "" {
		out += "\n"
	}
	_ = os.WriteFile(reaperPIDs, []byte(out), 0o600)
}
