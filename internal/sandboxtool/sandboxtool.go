// Package sandboxtool runs one tool, once, inside the pinned agent image: the
// PodmanAgent kind of the process-spawn law
// (https://docs.vornik.io §7.2–7.4).
//
// It is the single place such a one-shot is started. Tools that parse
// untrusted content (document rendering today; extractors and voice in S5b)
// never run on the daemon host, and every run is bounded the same way:
//
//   - no network, no pull, keep-id, no capabilities, no privilege escalation,
//     a read-only root with a bounded /tmp, a process limit;
//   - a memory limit and CPU share per FEATURE (not per binary: one binary
//     serves several features with different budgets), and a timeout;
//   - a bounded pool with one slot reserved for voice;
//   - inputs mounted read-only at /in, a fresh scratch /out, an optional
//     model directory read-only at /models;
//   - the scratch removed after every run and swept at startup;
//   - an outcome that tells a timeout from an OOM kill from a tool failure
//     from "not available in the agent image", counted in metrics.
//
// argv is built by the caller from typed options. The runner adds no user
// text; the caller must not either (tests pin it for each caller).
package sandboxtool

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/spawn"
)

// Feature names a use of the sandbox. Limits and timeouts are keyed by it.
type Feature string

// The features of §7.1, plus render (render_document, S4).
const (
	FeatureRender   Feature = "render"
	FeaturePDF      Feature = "pdf"
	FeatureImageOCR Feature = "image_ocr"
	FeatureVideo    Feature = "video"
	FeatureAudio    Feature = "audio"
	FeatureVoiceSTT Feature = "voice_stt"
	FeatureVoiceTTS Feature = "voice_tts"
)

type featureDefaults struct {
	memory  int64
	cpus    string
	timeout time.Duration
	// maxInput bounds a run's total input, refused before any container
	// starts; maxOutput bounds what the tool may leave in /out, checked after
	// the run (S5a review F3). Sized for the feature's real inputs: a voice
	// note is small, a video is not.
	maxInput  int64
	maxOutput int64
}

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// defaults are §7.2/§7.3's per-feature budgets. render is sized for pandoc
// with weasyprint. Output caps allow for what each feature legitimately
// writes: audio normalises to 16 kHz PCM, which is larger than its input.
var defaults = map[Feature]featureDefaults{
	FeatureRender:   {memory: 1 * gib, cpus: "1", timeout: 120 * time.Second, maxInput: 16 * mib, maxOutput: 256 * mib},
	FeaturePDF:      {memory: 512 * mib, cpus: "1", timeout: 60 * time.Second, maxInput: 256 * mib, maxOutput: 256 * mib},
	FeatureImageOCR: {memory: 512 * mib, cpus: "1", timeout: 120 * time.Second, maxInput: 32 * mib, maxOutput: 16 * mib},
	FeatureVideo:    {memory: 1 * gib, cpus: "2", timeout: 300 * time.Second, maxInput: 2 * gib, maxOutput: 256 * mib},
	FeatureAudio:    {memory: 2 * gib, cpus: "2", timeout: 600 * time.Second, maxInput: 512 * mib, maxOutput: 1 * gib},
	FeatureVoiceSTT: {memory: 2 * gib, cpus: "2", timeout: 120 * time.Second, maxInput: 64 * mib, maxOutput: 256 * mib},
	FeatureVoiceTTS: {memory: 2 * gib, cpus: "1", timeout: 60 * time.Second, maxInput: 16 * mib, maxOutput: 64 * mib},
}

// labelGated reports whether a feature's tool must be declared by the image's
// io.vornik.sandbox-tools label (§7.1 decision 4). render's pandoc predates
// the label and is not in it.
func labelGated(f Feature) bool { return f != FeatureRender }

// ToolsLabel is the agent image label that declares the sandbox tools.
const ToolsLabel = "io.vornik.sandbox-tools"

// RunLabel marks every container the runner starts, with a value naming this
// runner's scratch root, so a startup sweep removes only its own daemon's
// leftovers and never a bench daemon's run in flight (S5a review F1).
const RunLabel = "io.vornik.sandbox-run"

// labelRefresh is how often a label miss may re-read the image: a rebuilt
// image that gained a tool is picked up without a restart, and a burst of
// misses does not inspect the image per run.
const labelRefresh = 30 * time.Second

// Features lists every feature, for config validation and docs.
func Features() []Feature {
	return []Feature{FeatureRender, FeaturePDF, FeatureImageOCR, FeatureVideo,
		FeatureAudio, FeatureVoiceSTT, FeatureVoiceTTS}
}

// isVoice reports whether a feature is interactive (the reserved pool slot).
func isVoice(f Feature) bool { return f == FeatureVoiceSTT || f == FeatureVoiceTTS }

// DefaultMaxConcurrent is the pool size when none is configured.
const DefaultMaxConcurrent = 2

// FFmpegThreads caps ffmpeg's decoder, filter and encoder threads in every
// run. ffmpeg sizes its thread pools by the host's core count, not the
// container's --cpus, and a thread is a pid: on a 16-core host a video
// decode, its filter graph and its encoder together exceed --pids-limit=64,
// pthread_create fails, and ffmpeg retries until the timeout kills it
// (found by the S5b e2e lane, 2026-09-26). Callers pass it as both
// "-threads" and "-filter_threads" before -i, and "-threads" after.
const FFmpegThreads = "2"

// threadLimit is the OpenMP thread cap for a CPU share: the share rounded
// up, so tesseract's OpenMP pool fits the pids bound on any host.
func threadLimit(cpus string) string {
	v, err := strconv.ParseFloat(cpus, 64)
	if err != nil || v <= 0 {
		return "1"
	}
	return strconv.Itoa(int(math.Ceil(v)))
}

// DefaultScratchRoot is where per-run scratch lives when the caller names no
// root: uid-suffixed, so two users on one host never sweep each other's runs
// (S5a review F6). The daemon passes a root under its data directory.
func DefaultScratchRoot() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("vornik-sandbox-tools-%d", os.Getuid()))
}

// Config is the resolved configuration. Zero values take the defaults.
type Config struct {
	// Image is the pinned agent image. Empty: every run is not available.
	Image string
	// MaxConcurrent bounds concurrent runs; 0 takes DefaultMaxConcurrent.
	MaxConcurrent int
	// Limits, CPUs and Timeouts override a feature's defaults.
	Limits   map[Feature]int64
	CPUs     map[Feature]string
	Timeouts map[Feature]time.Duration
	// ScratchRoot holds per-run scratch; the runner owns everything in it.
	// Empty takes DefaultScratchRoot.
	ScratchRoot string
	// MaxInputBytes, when set, bounds every feature's total input; 0 keeps
	// each feature's own default.
	MaxInputBytes int64
}

// CommandRunner runs a program, with stdin when it is non-nil, and returns its
// combined output. Production uses ExecCommand; tests replace it or wrap it to
// observe argv.
type CommandRunner func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error)

// ExecCommand is the production CommandRunner. It runs podman only, through
// the process-spawn law's kinds (internal/spawn): a one-shot `run` is
// PodmanAgent, which refuses any image but the pinned agent image and any flag
// outside the closed grammar; the sweep, the label read, the OOM inspect and
// the removal are PodmanControl. A refused argv starts nothing.
func ExecCommand(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	var (
		cmd *spawn.Cmd
		err error
	)
	if len(args) > 0 && args[0] == "run" {
		cmd, err = spawn.PodmanAgent(ctx, name, args)
	} else {
		cmd, err = spawn.PodmanControl(ctx, name, args)
	}
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		cmd.SetStdin(stdin)
	}
	return cmd.CombinedOutput()
}

// Outcome is how a run ended.
type Outcome string

// The outcomes counted in vornik_sandbox_tool_runs_total.
const (
	OutcomeOK           Outcome = "ok"
	OutcomeFailed       Outcome = "failed"
	OutcomeTimeout      Outcome = "timeout"
	OutcomeOOM          Outcome = "oom"
	OutcomeNotAvailable Outcome = "not_available"
)

// ErrNotAvailable reports that the sandbox cannot run the tool at all: no image
// configured, podman absent, the image not present locally, or the tool missing
// from it. There is never a host fallback.
var ErrNotAvailable = errors.New("not available in the agent image")

// RunError is a run that did not end ok.
type RunError struct {
	Outcome Outcome
	Feature Feature
	Detail  string
}

func (e *RunError) Error() string {
	switch e.Outcome {
	case OutcomeTimeout:
		return fmt.Sprintf("%s timed out (%s)", e.Feature, e.Detail)
	case OutcomeOOM:
		return fmt.Sprintf("%s exceeded its memory limit (%s)", e.Feature, e.Detail)
	case OutcomeNotAvailable:
		return fmt.Sprintf("%s: %v (%s)", e.Feature, ErrNotAvailable, e.Detail)
	default:
		return fmt.Sprintf("%s failed: %s", e.Feature, e.Detail)
	}
}

// Is makes errors.Is(err, ErrNotAvailable) true for a not-available run.
func (e *RunError) Is(target error) bool {
	return target == ErrNotAvailable && e.Outcome == OutcomeNotAvailable
}

// Input is a file the daemon writes into /in, read-only to the tool: Data, or
// the host file at Path copied in (for inputs too large to hold in memory).
// Name is chosen by the caller, never taken from user input.
type Input struct {
	Name string
	Data []byte
	Path string
}

// Spec is one run. Entrypoint is fixed per feature by the caller, and Args are
// built by the caller from typed options: never user text.
type Spec struct {
	Feature    Feature
	Entrypoint string
	Args       []string
	Inputs     []Input
	// ModelDir, when set, is a host DIRECTORY mounted read-only at /models (a
	// directory, so a model's sibling files such as piper's .onnx.json come
	// with it).
	ModelDir string
	// Stdin, when set, names one of Inputs whose content is fed to the tool's
	// standard input (piper reads its text only from stdin).
	Stdin string
}

// Result is a finished run. OutDir holds what the tool wrote to /out until
// Close.
type Result struct {
	OutDir string
	Output []byte
	close  func()
}

// Close removes the run's scratch.
func (r *Result) Close() {
	if r != nil && r.close != nil {
		r.close()
		r.close = nil
	}
}

// Sandbox is what a feature needs from the runner: extractors and voice take
// it at construction, and a nil Sandbox means "not available", never a host
// fallback.
type Sandbox interface {
	Run(ctx context.Context, spec Spec) (*Result, error)
}

var _ Sandbox = (*Runner)(nil)

// Runner starts sandbox one-shots.
type Runner struct {
	cfg     Config
	run     CommandRunner
	pool    *pool
	metrics *Metrics
	log     zerolog.Logger
	scope   string // RunLabel value: this runner's scratch root, hashed

	toolsMu     sync.Mutex
	tools       map[string]bool // nil until the label has been read
	toolsErr    error
	toolsReadAt time.Time
	refreshing  chan struct{} // non-nil while a label read is in flight; closed when it lands
	now         func() time.Time

	outputSample time.Duration // how often /out is sized during a run
}

// New validates cfg and returns a Runner. An unknown feature key in Limits or
// Timeouts is refused: a misspelt key silently ignored would leave the default
// in force while the operator believes otherwise.
func New(cfg Config, run CommandRunner) (*Runner, error) {
	for f := range cfg.Limits {
		if _, ok := defaults[f]; !ok {
			return nil, fmt.Errorf("sandbox_tools.limits: unknown feature %q (known: %s)", f, featureList())
		}
	}
	for f := range cfg.Timeouts {
		if _, ok := defaults[f]; !ok {
			return nil, fmt.Errorf("sandbox_tools.timeouts: unknown feature %q (known: %s)", f, featureList())
		}
	}
	for f, c := range cfg.CPUs {
		if _, ok := defaults[f]; !ok {
			return nil, fmt.Errorf("sandbox_tools.cpus: unknown feature %q (known: %s)", f, featureList())
		}
		if v, err := strconv.ParseFloat(c, 64); err != nil || v <= 0 {
			return nil, fmt.Errorf("sandbox_tools.cpus.%s %q: must be a positive number of CPUs", f, c)
		}
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.MaxInputBytes < 0 {
		cfg.MaxInputBytes = 0
	}
	if cfg.ScratchRoot == "" {
		cfg.ScratchRoot = DefaultScratchRoot()
	}
	if run == nil {
		run = ExecCommand
	}
	sum := sha256.Sum256([]byte(cfg.ScratchRoot))
	return &Runner{
		cfg: cfg, run: run, pool: newPool(cfg.MaxConcurrent),
		log: zerolog.Nop(), scope: hex.EncodeToString(sum[:6]), now: time.Now,
		outputSample: time.Second,
	}, nil
}

func featureList() string {
	names := make([]string, 0, len(defaults))
	for _, f := range Features() {
		names = append(names, string(f))
	}
	return strings.Join(names, ", ")
}

// SetMetrics wires the metrics once observability exists.
func (r *Runner) SetMetrics(m *Metrics) { r.metrics = m }

// SetLogger sets where the runner reports what it cannot surface as a run's
// outcome: a container it failed to remove (S5a review F2).
func (r *Runner) SetLogger(l zerolog.Logger) { r.log = l }

// Image is the pinned image the runner starts.
func (r *Runner) Image() string { return r.cfg.Image }

// CPUs is a feature's effective CPU share, as podman's --cpus takes it.
func (r *Runner) CPUs(f Feature) string {
	if v, ok := r.cfg.CPUs[f]; ok && v != "" {
		return v
	}
	return defaults[f].cpus
}

// MaxInput is a feature's effective input bound in bytes.
func (r *Runner) MaxInput(f Feature) int64 {
	if r.cfg.MaxInputBytes > 0 {
		return r.cfg.MaxInputBytes
	}
	return defaults[f].maxInput
}

// MaxOutput is what a feature's tool may leave in /out.
func (r *Runner) MaxOutput(f Feature) int64 { return defaults[f].maxOutput }

// Memory is a feature's effective memory limit in bytes.
func (r *Runner) Memory(f Feature) int64 {
	if v, ok := r.cfg.Limits[f]; ok && v > 0 {
		return v
	}
	return defaults[f].memory
}

// MaxConcurrent is the effective pool size.
func (r *Runner) MaxConcurrent() int { return r.cfg.MaxConcurrent }

// LargestMemory is the largest effective per-feature memory limit: the most
// one slot can commit.
func (r *Runner) LargestMemory() int64 {
	var largest int64
	for _, f := range Features() {
		largest = max(largest, r.Memory(f))
	}
	return largest
}

// Committed is the most memory the pool can hold at once: every slot running
// the largest feature. The startup memory check adds it to the agents' share.
func (r *Runner) Committed() int64 {
	return int64(r.cfg.MaxConcurrent) * r.LargestMemory()
}

// Timeout is a feature's effective timeout.
func (r *Runner) Timeout(f Feature) time.Duration {
	if v, ok := r.cfg.Timeouts[f]; ok && v > 0 {
		return v
	}
	return defaults[f].timeout
}

// SweepScratch removes everything under the scratch root: at startup no run is
// in flight, so what is there was left by a process that died mid-run
// (S5-N4). Returns how many entries it removed.
func (r *Runner) SweepScratch() (int, error) {
	entries, err := os.ReadDir(r.cfg.ScratchRoot)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if rerr := os.RemoveAll(filepath.Join(r.cfg.ScratchRoot, e.Name())); rerr == nil {
			n++
		}
	}
	return n, nil
}

// SweepContainers removes the containers an earlier process of this daemon
// left: a run is named and not --rm, so a daemon killed mid-run leaves its
// container in podman's storage, which the in-process removal never reaches
// (S5a review F1, the container half of S5-N4). Only containers carrying this
// runner's RunLabel value are touched, so a bench daemon's runs in flight are
// not. Returns how many it removed.
func (r *Runner) SweepContainers(ctx context.Context) (int, error) {
	out, err := r.run(ctx, nil, "podman", "ps", "-a",
		"--filter", "label="+RunLabel+"="+r.scope, "--format", "{{.Names}}")
	if err != nil {
		return 0, fmt.Errorf("sandboxtool: list leftover containers: %w: %s", err, strings.TrimSpace(string(out)))
	}
	n := 0
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, containerPrefix) {
			continue
		}
		if r.remove(name) {
			n++
		}
	}
	return n, nil
}

// Scope is this runner's RunLabel value: what its runs carry and its sweep
// matches.
func (r *Runner) Scope() string { return r.scope }

// containerPrefix names every run's container.
const containerPrefix = "vornik-sbx-"

// LoadTools reads the pinned image's io.vornik.sandbox-tools label, which
// declares the tools the image carries (§7.1 decision 4). The daemon calls it
// at startup; a run of a gated feature whose tool is not declared reports
// "not available in the agent image" without starting a container, and a miss
// re-reads the label at most every labelRefresh, so a rebuilt image is picked
// up. An image inspect is a daemon-built argv against the pinned image.
func (r *Runner) LoadTools(ctx context.Context) (map[string]bool, error) {
	r.toolsMu.Lock()
	done := r.refreshLocked()
	r.toolsMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	return r.tools, r.toolsErr
}

// refreshLocked starts a label read unless one is in flight and returns the
// channel that closes when the read in flight lands. The read runs WITHOUT the
// lock and on its own context: holding the lock across a 30 s inspect stalled
// every feature, voice included (S5b review residual R3), and a caller whose
// context ends must not leave a cancelled read in the cache.
func (r *Runner) refreshLocked() chan struct{} {
	if r.refreshing != nil {
		return r.refreshing
	}
	done := make(chan struct{})
	r.refreshing = done
	go func() {
		tools, err := r.readLabel()
		r.toolsMu.Lock()
		// Stamped when the read lands, so a slow inspect does not pre-age it.
		r.tools, r.toolsErr, r.refreshing, r.toolsReadAt = tools, err, nil, r.now()
		r.toolsMu.Unlock()
		close(done)
	}()
	return done
}

func (r *Runner) readLabel() (map[string]bool, error) {
	if strings.TrimSpace(r.cfg.Image) == "" {
		return map[string]bool{}, errors.New("no agent image configured")
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := r.run(c, nil, "podman", "image", "inspect",
		"--format", `{{index .Labels "`+ToolsLabel+`"}}`, r.cfg.Image)
	if err != nil {
		return map[string]bool{}, fmt.Errorf("inspect %s: %s", r.cfg.Image, firstLine(strings.TrimSpace(string(out)), err))
	}
	tools := map[string]bool{}
	value := strings.TrimSpace(string(out))
	if value != "<no value>" {
		for _, t := range strings.Split(value, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tools[t] = true
			}
		}
	}
	return tools, nil
}

// Declared reports whether the pinned image declares tool and, when not,
// why: for the readiness page and the doctor, which say what a run would.
func (r *Runner) Declared(ctx context.Context, tool string) (bool, string) {
	return r.toolDeclared(ctx, tool)
}

// toolDeclared reports whether the image declares tool, re-reading the label
// on a miss when the last read is older than labelRefresh. A hit answers from
// the last read and never waits; a miss, the only answer a re-read can change,
// waits for the read in flight, bounded by its own context (S5b review
// residual R3). The detail says why a tool is not available.
func (r *Runner) toolDeclared(ctx context.Context, tool string) (bool, string) {
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	if !r.tools[tool] && (r.tools == nil || r.refreshing != nil || r.now().Sub(r.toolsReadAt) >= labelRefresh) {
		done := r.refreshLocked()
		r.toolsMu.Unlock()
		select {
		case <-done:
			r.toolsMu.Lock()
		case <-ctx.Done():
			r.toolsMu.Lock()
			if r.tools == nil {
				return false, fmt.Sprintf("the %s label of %s is still being read: %v", ToolsLabel, r.cfg.Image, ctx.Err())
			}
		}
	}
	if r.tools[tool] {
		return true, ""
	}
	if r.toolsErr != nil {
		return false, r.toolsErr.Error()
	}
	if len(r.tools) == 0 {
		return false, fmt.Sprintf("%s declares no sandbox tools (%s label absent: an image older than this release)", r.cfg.Image, ToolsLabel)
	}
	return false, fmt.Sprintf("%s does not declare %s in its %s label", r.cfg.Image, tool, ToolsLabel)
}

// Run starts one tool in the sandbox and waits for it.
func (r *Runner) Run(ctx context.Context, spec Spec) (res *Result, err error) {
	var ran time.Duration
	defer func() { r.observe(spec.Feature, err, ran) }()

	if _, ok := defaults[spec.Feature]; !ok {
		return nil, fmt.Errorf("sandboxtool: unknown feature %q", spec.Feature)
	}
	if strings.TrimSpace(r.cfg.Image) == "" {
		return nil, &RunError{Outcome: OutcomeNotAvailable, Feature: spec.Feature, Detail: "no agent image configured"}
	}
	if spec.Entrypoint == "" || strings.ContainsAny(spec.Entrypoint, "/ ") {
		return nil, fmt.Errorf("sandboxtool: entrypoint %q must be a bare program name", spec.Entrypoint)
	}
	total, err := inputSize(spec)
	if err != nil {
		return nil, err
	}
	if limit := r.MaxInput(spec.Feature); total > limit {
		return nil, fmt.Errorf("sandboxtool: input too large (%d bytes, limit %d)", total, limit)
	}
	if labelGated(spec.Feature) {
		if ok, detail := r.toolDeclared(ctx, spec.Entrypoint); !ok {
			return nil, &RunError{Outcome: OutcomeNotAvailable, Feature: spec.Feature, Detail: detail}
		}
	}

	waitStart := time.Now()
	release, err := r.pool.acquire(ctx, spec.Feature)
	if err != nil {
		return nil, err
	}
	defer release()
	r.observeWait(spec.Feature, time.Since(waitStart))
	start := time.Now()
	defer func() { ran = time.Since(start) }()

	work, inDir, outDir, err := r.scratch(spec.Inputs, r.MaxInput(spec.Feature))
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work) }

	var stdin io.Reader
	if spec.Stdin != "" {
		f, oerr := os.Open(filepath.Join(inDir, spec.Stdin))
		if oerr != nil {
			cleanup()
			return nil, oerr
		}
		defer func() { _ = f.Close() }()
		stdin = f
	}

	name := containerPrefix + randomHex()
	args := r.argv(spec, name, inDir, outDir)

	runCtx, cancel := context.WithTimeout(ctx, r.Timeout(spec.Feature))
	defer cancel()
	stopWatch, overflowed := r.watchOutput(outDir, r.MaxOutput(spec.Feature), name)
	defer stopWatch() // a panicking run must not strand the sampler
	out, runErr := r.run(runCtx, stdin, "podman", args...)
	stopWatch()
	if overflowed() {
		cleanup()
		r.remove(name)
		return nil, &RunError{Outcome: OutcomeFailed, Feature: spec.Feature,
			Detail: fmt.Sprintf("output exceeded the %s limit during the run", humanBytes(r.MaxOutput(spec.Feature)))}
	}
	if runErr == nil {
		return r.collect(spec.Feature, name, outDir, out, cleanup)
	}
	defer cleanup()
	defer r.remove(name)
	return nil, r.classify(ctx, runCtx, spec, name, out, runErr)
}

// collect finishes a run that exited cleanly: the container is removed and
// the output is refused if it outgrew the feature's cap after the run.
func (r *Runner) collect(f Feature, name, outDir string, out []byte, cleanup func()) (*Result, error) {
	r.remove(name)
	if size := dirSize(outDir); size > r.MaxOutput(f) {
		cleanup()
		return nil, &RunError{Outcome: OutcomeFailed, Feature: f,
			Detail: fmt.Sprintf("output of %d bytes exceeds the %s limit", size, humanBytes(r.MaxOutput(f)))}
	}
	return &Result{OutDir: outDir, Output: out, close: cleanup}, nil
}

// inputSize validates a spec's inputs and sums their sizes, before any
// container starts.
func inputSize(spec Spec) (int64, error) {
	var total int64
	stdinFound := spec.Stdin == ""
	for _, in := range spec.Inputs {
		if in.Name == "" || in.Name == "." || in.Name == ".." || strings.ContainsAny(in.Name, `/\`) {
			return 0, fmt.Errorf("sandboxtool: input name %q must be a plain file name", in.Name)
		}
		if in.Name == spec.Stdin {
			stdinFound = true
		}
		if in.Path == "" {
			total += int64(len(in.Data))
			continue
		}
		st, err := os.Stat(in.Path)
		if err != nil {
			return 0, fmt.Errorf("sandboxtool: input %s: %w", in.Name, err)
		}
		total += st.Size()
	}
	if !stdinFound {
		return 0, fmt.Errorf("sandboxtool: stdin %q names no input", spec.Stdin)
	}
	return total, nil
}

// watchOutput sizes outDir every outputSample while a run is in flight and,
// at the first sample over limit, removes the container, which ends the run
// (S5b review residual R1: the post-run check alone let a tool fill the host
// disk inside its timeout). The overshoot is at most one interval's writes;
// --ulimit fsize bounds any single file. stop ends the watch and waits for it;
// it may be called more than once.
func (r *Runner) watchOutput(outDir string, limit int64, name string) (stop func(), overflowed func() bool) {
	var over atomic.Bool
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(r.outputSample)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				if dirSize(outDir) > limit {
					over.Store(true)
					r.remove(name)
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-done }) }, over.Load
}

// dirSize sums the regular files under dir: the post-run /out bound (S5a
// review F3). A tool that fills the disk inside its timeout is refused here,
// before any caller reads the output.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (r *Runner) scratch(inputs []Input, limit int64) (work, inDir, outDir string, err error) {
	if err := os.MkdirAll(r.cfg.ScratchRoot, 0o700); err != nil {
		return "", "", "", err
	}
	work, err = os.MkdirTemp(r.cfg.ScratchRoot, "run-*")
	if err != nil {
		return "", "", "", err
	}
	inDir, outDir = filepath.Join(work, "in"), filepath.Join(work, "out")
	for _, d := range []string{inDir, outDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			_ = os.RemoveAll(work)
			return "", "", "", err
		}
	}
	for _, in := range inputs {
		dst := filepath.Join(inDir, in.Name)
		var werr error
		if in.Path != "" {
			werr = copyCapped(in.Path, dst, limit)
		} else {
			werr = os.WriteFile(dst, in.Data, 0o600)
		}
		if werr != nil {
			_ = os.RemoveAll(work)
			return "", "", "", werr
		}
	}
	return work, inDir, outDir, nil
}

// copyCapped copies a host input into scratch, refusing a file that grew past
// the input bound after it was sized.
func copyCapped(src, dst string, limit int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("sandboxtool: input too large (over %d bytes)", limit)
	}
	return err
}

// argv is the one fixed shape every sandbox one-shot takes (§7.2).
func (r *Runner) argv(spec Spec, name, inDir, outDir string) []string {
	mem := strconv.FormatInt(r.Memory(spec.Feature), 10)
	args := []string{
		"run", "--name", name, "--label", RunLabel + "=" + r.scope,
		"--network=none", "--pull=never", "--userns=keep-id",
		"--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,size=256m",
		"--memory=" + mem, "--memory-swap=" + mem,
		"--pids-limit=64", "--cpus=" + r.CPUs(spec.Feature),
		// No single file may outgrow the feature's output cap; the kernel
		// refuses the write (S5b review residual R1).
		"--ulimit", "fsize=" + strconv.FormatInt(r.MaxOutput(spec.Feature), 10),
		// A read-only root has no writable home; tools that cache (fontconfig
		// under weasyprint) get /tmp.
		"-e", "HOME=/tmp", "-e", "XDG_CACHE_HOME=/tmp",
		// OpenMP (tesseract) sizes its pool by the host's cores; cap it to
		// the CPU share so its threads fit the pids bound.
		"-e", "OMP_THREAD_LIMIT=" + threadLimit(r.CPUs(spec.Feature)),
		"-v", inDir + ":/in:ro,Z",
		"-v", outDir + ":/out:Z",
	}
	if spec.ModelDir != "" {
		args = append(args, "-v", spec.ModelDir+":/models:ro,Z")
	}
	if spec.Stdin != "" {
		args = append(args, "-i")
	}
	args = append(args, "-w", "/out", "--entrypoint", spec.Entrypoint, r.cfg.Image)
	return append(args, spec.Args...)
}

// classify turns a failed run into its outcome. The container still exists
// (no --rm), so an OOM kill is read from the kernel's own verdict.
func (r *Runner) classify(ctx, runCtx context.Context, spec Spec, name string, out []byte, runErr error) error {
	body := strings.TrimSpace(string(out))
	switch {
	case errors.Is(runErr, spawn.ErrNotFound):
		return &RunError{Outcome: OutcomeNotAvailable, Feature: spec.Feature, Detail: "podman is not installed"}
	case ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// A tool at its memory limit can thrash until the deadline: the
		// kernel's verdict decides, so the oom counter still sees it (S5a
		// review F4). The deadline ended the podman client, not the
		// container, so stop it first and read a final verdict (S5b review
		// residual R2).
		r.stop(name)
		if r.oomKilled(name) {
			return &RunError{Outcome: OutcomeOOM, Feature: spec.Feature,
				Detail: humanBytes(r.Memory(spec.Feature)) + ", then timed out"}
		}
		return &RunError{Outcome: OutcomeTimeout, Feature: spec.Feature, Detail: "after " + r.Timeout(spec.Feature).String()}
	case ctx.Err() != nil:
		return fmt.Errorf("sandboxtool: %s cancelled: %w", spec.Feature, ctx.Err())
	case notAvailableOutput(body):
		return &RunError{Outcome: OutcomeNotAvailable, Feature: spec.Feature, Detail: firstLine(body, runErr)}
	case r.oomKilled(name):
		return &RunError{Outcome: OutcomeOOM, Feature: spec.Feature, Detail: humanBytes(r.Memory(spec.Feature))}
	}
	detail := body
	if detail == "" {
		detail = runErr.Error()
	}
	return &RunError{Outcome: OutcomeFailed, Feature: spec.Feature, Detail: detail}
}

// notAvailableMarkers are podman's own messages for an image it cannot find
// and an entrypoint missing from the image. Generic exit codes are NOT markers:
// a tool that itself exits 125 or 127 is a failure to report, not a missing
// image (S4 review F6).
var notAvailableMarkers = []string{
	"image not known",    // podman: image absent from local storage (--pull=never)
	"no such image",      // podman: image absent, older wording
	"short-name",         // podman: unqualified name with no resolution
	"not found in $path", // crun/runc: the entrypoint is not in the image
}

func notAvailableOutput(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range notAvailableMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// stop kills a run's container at once and waits for it to exit.
func (r *Runner) stop(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = r.run(ctx, nil, "podman", "stop", "-t", "0", "--ignore", name)
}

func (r *Runner) oomKilled(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := r.run(ctx, nil, "podman", "inspect", "--format", "{{.State.OOMKilled}}", name)
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// remove deletes a run's container. A failure is logged and counted, never
// dropped: the container may still hold its cgroup, and the next startup's
// SweepContainers is what reclaims it (S5a review F2).
func (r *Runner) remove(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := r.run(ctx, nil, "podman", "rm", "-f", "--ignore", name)
	if err == nil {
		return true
	}
	r.log.Warn().Err(err).Str("container", name).Str("output", strings.TrimSpace(string(out))).
		Msg("sandbox tools: could not remove a run's container; the next daemon start sweeps it")
	if r.metrics != nil {
		r.metrics.removeFailures.Inc()
	}
	return false
}

func (r *Runner) observe(f Feature, err error, d time.Duration) {
	if r.metrics == nil {
		return
	}
	outcome := OutcomeOK
	var re *RunError
	switch {
	case errors.As(err, &re):
		outcome = re.Outcome
	case err != nil:
		outcome = OutcomeFailed
	}
	r.metrics.observe(f, outcome, d)
}

func (r *Runner) observeWait(f Feature, d time.Duration) {
	if r.metrics != nil {
		r.metrics.wait.WithLabelValues(string(f)).Observe(d.Seconds())
	}
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func firstLine(body string, err error) string {
	if body == "" {
		return err.Error()
	}
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		return body[:i]
	}
	return body
}

func humanBytes(n int64) string {
	switch {
	case n >= gib && n%gib == 0:
		return fmt.Sprintf("%dGiB", n/gib)
	case n >= mib:
		return fmt.Sprintf("%dMiB", n/mib)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
