package api

// POST /api/v1/setup/apply — one restart, done by the daemon, so the
// first-run page needs no shell (installation-onboarding-guide-design.md,
// "Amendment 2026-11-14 (#758)", W1). It restarts only an idle daemon that a
// supervisor declares it will start again; it starts no process (the
// spawn law): the daemon exits and systemd's Restart=on-failure brings it
// back.

import (
	"errors"
	"net/http"
	"os"

	"vornik.io/vornik/internal/configlock"
)

// WithRestartRequester wires the container's restart request: it cancels the
// daemon's run context, as SIGTERM does, and makes Run return the restart
// sentinel. Nil leaves POST /api/v1/setup/apply answering 503.
func WithRestartRequester(fn func()) ServerOption {
	return func(s *Server) { s.restartRequester = fn }
}

// supervisedRestart reports whether a supervisor will start the daemon again
// after it exits: systemd's INVOCATION_ID plus the shipped unit's own
// declaration that it restarts (VORNIK_RESTART_ON_EXIT=1, beside
// Restart=on-failure in deployments/podman/systemd/vornik.service).
func (s *Server) supervisedRestart() bool {
	env := s.setupEnv
	if env == nil {
		env = os.Getenv
	}
	return env("INVOCATION_ID") != "" && env("VORNIK_RESTART_ON_EXIT") == "1"
}

// SetupApply handles POST /api/v1/setup/apply. The checks run in the order
// the design gives; every refusal releases what it took.
func (s *Server) SetupApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if s.restartRequester == nil {
		respondError(w, http.StatusServiceUnavailable, "RESTART_NOT_WIRED", "this daemon cannot restart itself")
		return
	}
	if !s.setupApplying.CompareAndSwap(false, true) {
		respondError(w, http.StatusConflict, "ALREADY_APPLYING", "already applying; wait for the daemon to come back")
		return
	}
	refuse := func(code int, errCode, msg string) {
		s.setupApplying.Store(false)
		respondError(w, code, errCode, msg)
	}
	if !s.supervisedRestart() {
		refuse(http.StatusConflict, "NO_SUPERVISOR", "this daemon does not run under a supervisor that restarts it; restart it from a shell: systemctl --user restart vornik")
		return
	}
	// A daemon built without a task repository (tools, some tests) has no
	// task state to consult; the daemon Run builds always has one.
	if s.taskRepo != nil {
		busy, err := taskListerAdapter{repo: s.taskRepo}.HasActiveTasks(r.Context())
		if err != nil {
			refuse(http.StatusInternalServerError, "TASKS_UNREADABLE", "could not check for running jobs")
			return
		}
		if busy {
			refuse(http.StatusConflict, "JOB_RUNNING", "a job is running; try again when it finishes")
			return
		}
	}
	unlock, err := configlock.LockAsWriter(r.Context())
	if err != nil {
		if errors.Is(err, configlock.ErrNotConfigWriter) {
			refuse(http.StatusConflict, "NOT_CONFIG_WRITER", "this daemon does not hold the config-writer lease")
			return
		}
		refuse(http.StatusServiceUnavailable, "CONFIG_LOCK", "the config lock could not be taken")
		return
	}
	// Held until the process exits: no config write starts after this.
	s.setupApplyUnlock = unlock
	s.logger.Info().Msg("setup apply: restarting the daemon to apply the setup")
	respondJSON(w, http.StatusAccepted, map[string]any{"applying": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Outside the request: the reply is out before the shutdown begins.
	go s.restartRequester()
}

// releaseSetupApply undoes a successful apply's hold (tests only: in
// production the process exits).
func (s *Server) releaseSetupApply() {
	if s.setupApplyUnlock != nil {
		s.setupApplyUnlock()
		s.setupApplyUnlock = nil
	}
	s.setupApplying.Store(false)
}
