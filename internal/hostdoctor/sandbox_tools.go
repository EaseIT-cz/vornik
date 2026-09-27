package hostdoctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/imagemanifest"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/probe"
)

// SandboxEnv is what the sandbox_tools check needs from the daemon config:
// the sandbox limits (so the probes run with the daemon's flags) and the
// effective model paths.
type SandboxEnv struct {
	Tools  config.SandboxToolsConfig
	Models probe.Models
}

// WithSandbox enables the sandbox_tools check with the daemon config's
// values. Without it the check reports SKIPPED.
func (h *Checker) WithSandbox(env SandboxEnv) *Checker {
	h.sandboxEnv = &env
	return h
}

// sandboxOutcome is what the probe seam returns: the tools the pinned image
// declares (or why they could not be read), and each probe's verdict.
type sandboxOutcome struct {
	tools   map[string]bool
	loadErr error
	results []probe.Result
}

// doctorScratchRoot keeps the doctor's runs apart from any daemon's, so a
// daemon's startup sweep never removes a doctor run in flight, nor the
// reverse.
func doctorScratchRoot() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("vornik-doctor-sandbox-%d", os.Getuid()))
}

// realSandboxProbe builds a runner exactly as the daemon does (same
// FromConfig, same pinned image) and runs every probe through it.
func realSandboxProbe(ctx context.Context, env SandboxEnv) sandboxOutcome {
	cfg, err := sandboxtool.FromConfig(env.Tools, imagemanifest.AgentImageTag, doctorScratchRoot())
	if err != nil {
		return sandboxOutcome{loadErr: fmt.Errorf("sandbox_tools config: %w", err)}
	}
	r, err := sandboxtool.New(cfg, nil)
	if err != nil {
		return sandboxOutcome{loadErr: err}
	}
	tools, err := r.LoadTools(ctx)
	if err != nil || len(tools) == 0 {
		return sandboxOutcome{tools: tools, loadErr: err}
	}
	return sandboxOutcome{tools: tools, results: probe.Run(ctx, r, env.Models)}
}

// checkSandboxTools runs each sandbox tool on a fixture (design §7.1
// decision 4): presence is not function (S5-F10).
func (h *Checker) checkSandboxTools(ctx context.Context) Check {
	name := "sandbox_tools"
	if h.sandboxEnv == nil {
		return Check{Name: name, Status: "SKIPPED", Message: "daemon config not loaded; cannot tell which models to probe"}
	}
	run := h.sandboxProbeFunc
	if run == nil {
		run = realSandboxProbe
	}
	out := run(ctx, *h.sandboxEnv)
	if out.loadErr != nil {
		return Check{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"cannot read the agent image (%v): PDF, OCR, video, audio and voice report \"not available\" until it exists (pull it, or rebuild with `make build-agent`)", out.loadErr)}
	}
	if len(out.tools) == 0 {
		return Check{Name: name, Status: "WARNING", Message: fmt.Sprintf(
			"the agent image %s declares no sandbox tools (%s label absent: an image older than this release); PDF, OCR, video, audio and voice report \"not available\" — pull the release image, or rebuild with `make build-agent`",
			imagemanifest.AgentImageTag, sandboxtool.ToolsLabel)}
	}

	status := "OK"
	var failed, absent int
	items := make([]string, 0, len(out.results))
	for _, r := range out.results {
		items = append(items, fmt.Sprintf("%s %s: %s", r.Status, r.Name, r.Detail))
		switch r.Status {
		case probe.StatusFailed:
			failed++
		case probe.StatusNotAvailable:
			absent++
		}
	}
	switch {
	case failed > 0:
		status = "ERROR"
	case absent > 0:
		status = "WARNING"
	}
	declared := make([]string, 0, len(out.tools))
	for t := range out.tools {
		declared = append(declared, t)
	}
	sort.Strings(declared)
	msg := fmt.Sprintf("%d probes run in the sandbox (image declares %s): %d failed, %d not available",
		len(out.results), strings.Join(declared, ","), failed, absent)
	return Check{Name: name, Status: status, Message: msg, Items: items}
}
