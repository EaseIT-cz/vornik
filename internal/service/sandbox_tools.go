package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/imagemanifest"
	"vornik.io/vornik/internal/sandboxtool"
)

// resolveSandboxTools turns the sandbox_tools block into the runner's config
// (process-spawn law S5a); sandboxtool.FromConfig holds the rules, shared
// with `vornikctl doctor`.
func resolveSandboxTools(sc config.SandboxToolsConfig, image, scratchRoot string) (sandboxtool.Config, error) {
	return sandboxtool.FromConfig(sc, image, scratchRoot)
}

// sandboxScratchRoot is where per-run scratch lives: under the data directory
// when there is one, so two daemons on one host (production and a bench) never
// sweep each other's runs at startup.
func sandboxScratchRoot() string {
	if dataDir := strings.TrimSpace(os.Getenv("VORNIK_DATA_DIR")); dataDir != "" {
		return filepath.Join(dataDir, "sandbox-tools")
	}
	return sandboxtool.DefaultScratchRoot()
}

// sandboxCommand is the runner's command seam: nil runs podman. Tests replace
// it so initSandboxTools never shells out.
var sandboxCommand sandboxtool.CommandRunner

// initSandboxTools builds the sandbox one-shot runner, sweeps the scratch and
// the containers an earlier process left behind (S5-N4, S5a review F1), and
// reads which tools the pinned image declares (§7.1 decision 4). A
// configuration it cannot honour fails startup; a missing image or label does
// not, since its features then report "not available" on their own.
func (c *Container) initSandboxTools() error {
	cfg, err := resolveSandboxTools(c.Config.SandboxTools, imagemanifest.AgentImageTag, sandboxScratchRoot())
	if err != nil {
		return err
	}
	r, err := sandboxtool.New(cfg, sandboxCommand)
	if err != nil {
		return err
	}
	r.SetLogger(c.Logger)
	if n, err := r.SweepScratch(); err != nil {
		c.Logger.Warn().Err(err).Msg("sandbox tools: scratch sweep failed")
	} else if n > 0 {
		c.Logger.Info().Int("removed", n).Msg("sandbox tools: removed scratch left by an earlier process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if n, err := r.SweepContainers(ctx); err != nil {
		c.Logger.Warn().Err(err).Msg("sandbox tools: container sweep failed")
	} else if n > 0 {
		c.Logger.Info().Int("removed", n).Msg("sandbox tools: removed containers left by an earlier process")
	}
	c.logSandboxTools(ctx, r)
	if cfg.MaxConcurrent == 1 {
		c.Logger.Warn().Msg("sandbox_tools.max_concurrent is 1: no slot is reserved for voice, " +
			"so a voice reply may wait behind an upload")
	}
	c.sandboxRunner = r
	return nil
}

// logSandboxTools reads and logs the tools the pinned image declares, so the
// startup log says up front which features will report "not available".
func (c *Container) logSandboxTools(ctx context.Context, r *sandboxtool.Runner) {
	tools, err := r.LoadTools(ctx)
	if err != nil {
		c.Logger.Warn().Err(err).Str("image", r.Image()).
			Msg("sandbox tools: cannot read the agent image; PDF, OCR, video, audio and voice report not available until it exists")
		return
	}
	if len(tools) == 0 {
		c.Logger.Warn().Str("image", r.Image()).
			Msgf("sandbox tools: the agent image declares no sandbox tools (%s label absent, an image older than this release); PDF, OCR, video, audio and voice report not available", sandboxtool.ToolsLabel)
		return
	}
	names := make([]string, 0, len(tools))
	for t := range tools {
		names = append(names, t)
	}
	sort.Strings(names)
	c.Logger.Info().Str("image", r.Image()).Str("tools", strings.Join(names, ",")).
		Msg("sandbox tools: the agent image declares its tools")
}
