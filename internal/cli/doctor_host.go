package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/hostdoctor"
)

// Host checks (process-spawn law, S2,
// https://docs.vornik.io). The daemon's
// doctor no longer runs podman, skopeo, systemctl or git: it reports a
// "host_checks" pointer instead, and vornikctl — the one process the ruling
// allows to spawn — runs those checks here, on the host, and merges them into
// the report in the pointer's place.

const hostChecksPointer = "host_checks"

// doctorHostChecks runs the host checks locally. daemonRevision is the
// daemon's build revision from its report, which the image-freshness check
// compares the host's images against. A seam so tests never shell out.
var doctorHostChecks = func(ctx context.Context, daemonRevision string) []doctorCheck {
	env := loadDoctorHostEnv()
	var out []doctorCheck
	checker := hostdoctor.New(env.configsDir, env.usernsMode, daemonRevision)
	if env.sandbox != nil {
		checker = checker.WithSandbox(*env.sandbox)
	}
	if env.runtime != nil {
		checker = checker.WithRuntime(*env.runtime)
	}
	for _, c := range checker.Run(ctx) {
		out = append(out, doctorCheck{Name: c.Name, Status: c.Status, Message: c.Message, Items: c.Items, Fixed: c.Fixed})
	}
	return out
}

// doctorWorktreeGitCleanup cleans git's side of the orphan worktrees the
// daemon's fix removed. A seam so tests never shell out.
var doctorWorktreeGitCleanup = func(removed []string) []string {
	return hostdoctor.CleanWorktreeGit(context.Background(), loadDoctorHostEnv().workspacesRoot, removed)
}

type doctorHostEnv struct {
	configsDir     string
	usernsMode     string
	workspacesRoot string
	sandbox        *hostdoctor.SandboxEnv
	runtime        *hostdoctor.RuntimeEnv
}

var (
	doctorHostEnvOnce sync.Once
	doctorHostEnvVal  doctorHostEnv
)

// loadDoctorHostEnv reads the daemon config the way every other vornikctl
// command does. A config that does not load leaves the checks that need it to
// report SKIPPED, rather than failing the whole doctor.
func loadDoctorHostEnv() doctorHostEnv {
	doctorHostEnvOnce.Do(func() {
		cfg, path, err := config.Load()
		doctorHostEnvVal.configsDir = resolveConfigsDir(path)
		if err == nil && cfg != nil {
			doctorHostEnvVal.usernsMode = cfg.Runtime.UserNSMode
			doctorHostEnvVal.workspacesRoot = cfg.Runtime.ProjectWorkspacePath
			env := sandboxEnvFromConfig(cfg)
			doctorHostEnvVal.sandbox = &env
			rt := runtimeEnvFromConfig(cfg)
			doctorHostEnvVal.runtime = &rt
		}
	})
	return doctorHostEnvVal
}

// sandboxEnvFromConfig is what the sandbox_tools probes need from the daemon
// config (process-spawn law S5b): its sandbox limits, and the effective
// models — the voice models when their provider is on, and the audio
// extraction model (extractors.audio.model_path, else voice.stt.model).
func sandboxEnvFromConfig(cfg *config.Config) hostdoctor.SandboxEnv {
	env := hostdoctor.SandboxEnv{Tools: cfg.SandboxTools}
	if strings.TrimSpace(cfg.Voice.STT.Provider) != "" {
		env.Models.VoiceSTT = strings.TrimSpace(cfg.Voice.STT.Model)
	}
	if strings.TrimSpace(cfg.Voice.TTS.Provider) != "" {
		env.Models.VoiceTTS = strings.TrimSpace(cfg.Voice.TTS.Voice)
	}
	env.Models.AudioExtraction = cfg.AudioExtractionModel()
	return env
}

// runtimeEnvFromConfig is what agent_image_uid needs from the daemon config
// (onboarding-hardening-design.md D5 R6): the configured run_as_user, and the
// directories whose owners the agent must be able to read.
func runtimeEnvFromConfig(cfg *config.Config) hostdoctor.RuntimeEnv {
	return hostdoctor.RuntimeEnv{
		RunAsUser:            strings.TrimSpace(cfg.Runtime.RunAsUser),
		ProjectWorkspacePath: cfg.Runtime.ProjectWorkspaceDir(),
		DependencyCacheDir:   cfg.Runtime.DependencyCacheDir(),
	}
}

// doctorContext is the command's context, or Background when the command was
// run outside cobra's Execute (tests, direct calls), where it is nil.
func doctorContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

// mergeHostChecks replaces the daemon's host_checks pointer with the checks run
// locally. A daemon from before S2 sends no pointer because it still runs those
// checks itself; then nothing runs locally, so no check appears twice.
func mergeHostChecks(ctx context.Context, report doctorReport) doctorReport {
	var merged []doctorCheck
	for _, c := range report.Checks {
		if c.Name == hostChecksPointer {
			merged = append(merged, doctorHostChecks(ctx, report.DaemonRevision)...)
			continue
		}
		merged = append(merged, c)
	}
	report.Checks = merged
	report.Summary = doctorSummary(merged)
	return report
}

// mergeHostChecksJSON does the same on the daemon's raw body, so --json keeps
// every field the daemon sent, including ones this CLI has not heard of.
func mergeHostChecksJSON(ctx context.Context, body []byte, daemonRevision string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	// A missing or null `checks` merges to an empty list: unmarshalling a
	// missing RawMessage fails with "unexpected end of JSON input".
	var raw []json.RawMessage
	if c := doc["checks"]; len(c) > 0 {
		if err := json.Unmarshal(c, &raw); err != nil {
			return nil, err
		}
	}
	merged := []json.RawMessage{}
	var statuses []doctorCheck
	for _, r := range raw {
		var head doctorCheck
		if err := json.Unmarshal(r, &head); err != nil {
			return nil, err
		}
		if head.Name != hostChecksPointer {
			merged = append(merged, r)
			statuses = append(statuses, head)
			continue
		}
		for _, local := range doctorHostChecks(ctx, daemonRevision) {
			enc, err := json.Marshal(local)
			if err != nil {
				return nil, err
			}
			merged = append(merged, enc)
			statuses = append(statuses, local)
		}
	}
	checks, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	doc["checks"] = checks
	summary, _ := json.Marshal(doctorSummary(statuses))
	doc["summary"] = summary
	return json.Marshal(doc)
}

// doctorSummary counts issues the way the daemon does: OK and SKIPPED are not
// issues.
func doctorSummary(checks []doctorCheck) string {
	issues := 0
	for _, c := range checks {
		if c.Status != "OK" && c.Status != "SKIPPED" {
			issues++
		}
	}
	if issues == 0 {
		return "All checks passed"
	}
	return fmt.Sprintf("%d issues found", issues)
}

// removedWorktrees collects the worktrees the daemon's orphan_worktrees fix
// removed, for the git cleanup on the host.
func removedWorktrees(report doctorReport) []string {
	var out []string
	for _, c := range report.Checks {
		if c.Name == "orphan_worktrees" {
			out = append(out, c.Removed...)
		}
	}
	return out
}
