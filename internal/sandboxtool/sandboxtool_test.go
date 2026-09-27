package sandboxtool

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The one-shot sandbox runner (process-spawn law S5a,
// https://docs.vornik.io §7.2–7.4). The
// S4 code review (review-20260926-6880) found render_document's podman run had
// no memory limit, no hardening flags, no timeout, no concurrency bound, no
// scratch sweep and no metrics: a crafted document could exhaust the host.
// Every sandbox one-shot now goes through this runner, which enforces all of
// them in one place.

type call struct {
	name  string
	args  []string
	stdin string
}

// allTools is the label the S5b agent image carries.
const allTools = "pdftotext,tesseract,ffmpeg,ffprobe,whisper-cli,piper"

// fakePodman plays podman: it records every call, writes the /out file a tool
// would, and answers inspect with a configured OOM verdict.
type fakePodman struct {
	mu        sync.Mutex
	calls     []call
	runErr    error
	runOut    string
	oom       bool
	block     chan struct{} // when set, `run` blocks until closed or ctx ends
	started   chan struct{}
	produces  string
	size      int     // bytes of the produced file; 0 writes "out"
	label     *string // the tools label; nil answers allTools
	labelErr  error
	leftovers string // what `podman ps` lists
	rmErr     error

	writeFirst   bool // write the produced file BEFORE blocking: a tool filling /out
	rmUnblocks   bool // `rm` ends a blocked run, as killing the container does
	unblockOnce  sync.Once
	inspectBlock chan struct{} // when set, `image inspect` blocks until closed or ctx ends
}

func (f *fakePodman) produce(args []string) {
	if f.produces == "" {
		return
	}
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			if host, ok := strings.CutSuffix(args[i+1], ":/out:Z"); ok {
				data := []byte("out")
				if f.size > 0 {
					data = make([]byte, f.size)
				}
				_ = os.WriteFile(filepath.Join(host, f.produces), data, 0o600)
			}
		}
	}
}

func (f *fakePodman) run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	var in string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{name, append([]string(nil), args...), in})
	f.mu.Unlock()
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "image":
		f.mu.Lock()
		blk := f.inspectBlock
		f.mu.Unlock()
		if blk != nil {
			select {
			case <-blk:
			case <-ctx.Done():
				return []byte("Error: context canceled"), ctx.Err()
			}
		}
		if f.labelErr != nil {
			return []byte("Error: image not known"), f.labelErr
		}
		if f.label == nil {
			return []byte(allTools + "\n"), nil
		}
		return []byte(*f.label + "\n"), nil
	case "ps":
		return []byte(f.leftovers), nil
	case "inspect":
		if f.oom {
			return []byte("true\n"), nil
		}
		return []byte("false\n"), nil
	case "rm":
		if f.rmUnblocks && f.block != nil {
			f.unblockOnce.Do(func() { close(f.block) })
		}
		if f.rmErr != nil {
			return []byte("Error: storage wedged"), f.rmErr
		}
		return nil, nil
	case "run":
		if f.started != nil {
			f.started <- struct{}{}
		}
		if f.writeFirst {
			f.produce(args)
		}
		if f.block != nil {
			select {
			case <-f.block:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if f.runErr != nil {
			return []byte(f.runOut), f.runErr
		}
		if !f.writeFirst {
			f.produce(args)
		}
		return []byte(f.runOut), nil
	}
	return nil, nil
}

func (f *fakePodman) runCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "run" {
			out = append(out, c)
		}
	}
	return out
}

func newRunner(t *testing.T, f *fakePodman, mod func(*Config)) *Runner {
	t.Helper()
	cfg := Config{Image: "localhost/vornik-agent:test", ScratchRoot: t.TempDir()}
	if mod != nil {
		mod(&cfg)
	}
	r, err := New(cfg, f.run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func renderSpec() Spec {
	return Spec{
		Feature:    FeatureRender,
		Entrypoint: "pandoc",
		Args:       []string{"/in/input.md", "-o", "/out/output.html"},
		Inputs:     []Input{{Name: "input.md", Data: []byte("# Hi")}},
	}
}

func TestRun_CarriesEveryLimitAndHardeningFlag(t *testing.T) {
	f := &fakePodman{produces: "output.html"}
	r := newRunner(t, f, nil)
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if _, err := os.Stat(filepath.Join(res.OutDir, "output.html")); err != nil {
		t.Fatalf("the output the tool wrote must be readable: %v", err)
	}
	runs := f.runCalls()
	if len(runs) != 1 || runs[0].name != "podman" {
		t.Fatalf("want exactly one podman run, got %+v", f.calls)
	}
	joined := strings.Join(runs[0].args, " ")
	for _, want := range []string{
		"--network=none", "--pull=never", "--userns=keep-id", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--read-only", "--tmpfs /tmp:rw,size=256m",
		"--memory=1073741824", "--memory-swap=1073741824", "--pids-limit=64", "--cpus=1",
		"-e OMP_THREAD_LIMIT=1",
		":/in:ro,Z", ":/out:Z", "--entrypoint pandoc", "localhost/vornik-agent:test",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %s", want, joined)
		}
	}
}

func TestRun_LimitsAndTimeoutsAreKeyedByFeature(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, func(c *Config) {
		c.Limits = map[Feature]int64{FeatureAudio: 3 << 30}
	})
	spec := renderSpec()
	spec.Feature = FeatureAudio
	spec.Entrypoint = "whisper-cli"
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	joined := strings.Join(f.runCalls()[0].args, " ")
	if !strings.Contains(joined, "--memory=3221225472") {
		t.Fatalf("the configured audio limit must apply: %s", joined)
	}
	if got := r.Timeout(FeatureAudio); got != 600*time.Second {
		t.Fatalf("audio timeout default = %s, want 10m", got)
	}
	if got := r.Timeout(FeatureVoiceTTS); got != 60*time.Second {
		t.Fatalf("voice_tts timeout default = %s, want 60s", got)
	}
}

func TestRun_TimeoutIsReportedAsTimeoutAndTheContainerRemoved(t *testing.T) {
	f := &fakePodman{block: make(chan struct{})}
	r := newRunner(t, f, func(c *Config) {
		c.Timeouts = map[Feature]time.Duration{FeatureRender: 50 * time.Millisecond}
	})
	_, err := r.Run(context.Background(), renderSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeTimeout {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the message must say it timed out: %v", err)
	}
	if !removedContainer(f) {
		t.Fatalf("a timed-out container must be removed: %+v", f.calls)
	}
}

func TestRun_OOMIsDistinguishedFromAFailure(t *testing.T) {
	f := &fakePodman{runErr: errors.New("exit status 137"), oom: true}
	r := newRunner(t, f, nil)
	_, err := r.Run(context.Background(), renderSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeOOM {
		t.Fatalf("want oom, got %v", err)
	}
	if !strings.Contains(err.Error(), "exceeded its memory limit") {
		t.Fatalf("got %v", err)
	}
	if !removedContainer(f) {
		t.Fatal("the container must be removed after inspect")
	}
}

func TestRun_AToolFailureIsAFailureNotNotAvailable(t *testing.T) {
	// S4 review F6: generic exit codes (125/127) used to be read as "not
	// available", masking a real conversion error.
	f := &fakePodman{runErr: errors.New("exit status 127"), runOut: "pandoc: Could not parse YAML metadata"}
	r := newRunner(t, f, nil)
	_, err := r.Run(context.Background(), renderSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeFailed {
		t.Fatalf("a tool error is a failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "Could not parse YAML metadata") {
		t.Fatalf("the tool's own message must survive: %v", err)
	}
}

func TestRun_NotAvailableCases(t *testing.T) {
	cases := map[string]*fakePodman{
		"image missing": {runErr: errors.New("exit status 125"), runOut: "Error: localhost/vornik-agent:test: image not known"},
		"tool missing":  {runErr: errors.New("exit status 127"), runOut: `Error: crun: executable file "pandoc" not found in $PATH: No such file or directory`},
		"no podman":     {runErr: exec.ErrNotFound},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRunner(t, f, nil)
			_, err := r.Run(context.Background(), renderSpec())
			if !errors.Is(err, ErrNotAvailable) {
				t.Fatalf("want not available, got %v", err)
			}
		})
	}
	t.Run("no image configured", func(t *testing.T) {
		f := &fakePodman{}
		r := newRunner(t, f, func(c *Config) { c.Image = "" })
		if _, err := r.Run(context.Background(), renderSpec()); !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("want not available, got %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatal("nothing may run without an image")
		}
	})
}

func TestRun_OversizeInputIsRefusedBeforeAnyContainer(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, func(c *Config) { c.MaxInputBytes = 10 })
	spec := renderSpec()
	spec.Inputs = []Input{{Name: "input.md", Data: []byte(strings.Repeat("x", 11))}}
	if _, err := r.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want a size refusal, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("no container may start for an oversize input")
	}
}

func TestRun_InputNamesCannotEscape(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, nil)
	for _, name := range []string{"../x", "/etc/x", "a/b", "", ".."} {
		spec := renderSpec()
		spec.Inputs = []Input{{Name: name, Data: []byte("x")}}
		if _, err := r.Run(context.Background(), spec); err == nil {
			t.Errorf("input name %q must be refused", name)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("nothing may run")
	}
}

func TestRun_ScratchIsRemovedAfterEveryOutcome(t *testing.T) {
	root := t.TempDir()
	for _, f := range []*fakePodman{{}, {runErr: errors.New("exit status 1")}} {
		r, err := New(Config{Image: "img", ScratchRoot: root}, f.run)
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Run(context.Background(), renderSpec())
		if err == nil {
			res.Close()
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
}

// S5-N4: a daemon killed mid-run leaves scratch the in-process cleanup never
// reaches; the next start sweeps it.
func TestSweepScratch_RemovesWhatAnEarlierProcessLeft(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"run-a", "run-b"} {
		if err := os.MkdirAll(filepath.Join(root, d, "out"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Config{Image: "img", ScratchRoot: root}, (&fakePodman{}).run)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.SweepScratch()
	if err != nil || n != 2 {
		t.Fatalf("swept %d (%v), want 2", n, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("left: %v", entries)
	}
}

// §7.3 / S5-N2: one slot is reserved for voice when there are at least two, so
// a burst of uploads cannot delay a voice reply; with one slot they share it.
func TestPool_ReservesASlotForVoice(t *testing.T) {
	p := newPool(2)
	release1, err := p.acquire(context.Background(), FeaturePDF)
	if err != nil {
		t.Fatal(err)
	}
	// A second upload must wait: its only other slot is voice's.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(ctx, FeatureVideo); err == nil {
		t.Fatal("a second upload must not take the voice slot")
	}
	// Voice still gets in.
	releaseV, err := p.acquire(context.Background(), FeatureVoiceSTT)
	if err != nil {
		t.Fatalf("voice must get the reserved slot: %v", err)
	}
	releaseV()
	release1()

	shared := newPool(1)
	rel, err := shared.acquire(context.Background(), FeaturePDF)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if rel, err = shared.acquire(context.Background(), FeatureVoiceSTT); err != nil {
		t.Fatal("with one slot, voice and uploads share it")
	}
	rel()
}

func TestNew_RefusesAnUnknownFeatureKey(t *testing.T) {
	_, err := New(Config{Image: "img", ScratchRoot: t.TempDir(),
		Limits: map[Feature]int64{"renders": 1 << 30}}, (&fakePodman{}).run)
	if err == nil {
		t.Fatal("a misspelt feature key must be refused, not silently ignored")
	}
}

// The startup memory check sizes the pool from the CONFIGURED limits
// (design §7.3, S5-N3): every slot running the largest feature.
func TestRunner_CommittedUsesTheConfiguredLimits(t *testing.T) {
	r, err := New(Config{Image: "img", ScratchRoot: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxConcurrent() != DefaultMaxConcurrent || r.LargestMemory() != 2<<30 || r.Committed() != 2*(2<<30) {
		t.Fatalf("defaults: pool %d, largest %d, committed %d", r.MaxConcurrent(), r.LargestMemory(), r.Committed())
	}
	lowered := map[Feature]int64{FeatureAudio: 1 << 30, FeatureVoiceSTT: 1 << 30, FeatureVoiceTTS: 1 << 30}
	r, err = New(Config{Image: "img", ScratchRoot: t.TempDir(), MaxConcurrent: 3, Limits: lowered}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.LargestMemory() != 1<<30 || r.Committed() != 3<<30 {
		t.Fatalf("lowering the 2GiB features lowers the product: largest %d, committed %d", r.LargestMemory(), r.Committed())
	}
}

// Design §7.4: every run is counted by feature and outcome, and timed.
func TestRun_MetricsCountEveryOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	f := &fakePodman{produces: "output.html"}
	r := newRunner(t, f, func(c *Config) {
		c.Timeouts = map[Feature]time.Duration{FeatureRender: 20 * time.Millisecond}
	})
	r.SetMetrics(NewMetrics(reg))

	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	f.block = make(chan struct{})
	if _, err := r.Run(context.Background(), renderSpec()); err == nil {
		t.Fatal("want a timeout")
	}
	f.block = nil
	f.runErr, f.runOut = errors.New("exit status 1"), "pandoc: bad input"
	if _, err := r.Run(context.Background(), renderSpec()); err == nil {
		t.Fatal("want a failure")
	}
	r.cfg.Image = ""
	if _, err := r.Run(context.Background(), renderSpec()); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("want not available, got %v", err)
	}

	for outcome, want := range map[Outcome]float64{OutcomeOK: 1, OutcomeTimeout: 1, OutcomeFailed: 1, OutcomeNotAvailable: 1, OutcomeOOM: 0} {
		got := testutil.ToFloat64(r.metrics.runs.WithLabelValues(string(FeatureRender), string(outcome)))
		if got != want {
			t.Errorf("runs{render,%s} = %v, want %v", outcome, got, want)
		}
	}
	if n := testutil.CollectAndCount(r.metrics.duration); n != 1 {
		t.Errorf("duration series = %d, want 1", n)
	}
}

// A model is mounted as its DIRECTORY, read-only (design §7.2: piper needs its
// .onnx.json beside the .onnx).
func TestRun_ModelDirIsMountedReadOnly(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, nil)
	spec := renderSpec()
	spec.Feature, spec.Entrypoint, spec.ModelDir = FeatureVoiceTTS, "piper", "/models/piper"
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	joined := strings.Join(f.runCalls()[0].args, " ")
	if !strings.Contains(joined, "-v /models/piper:/models:ro,Z") || !strings.Contains(joined, "--memory=2147483648") {
		t.Fatalf("argv: %s", joined)
	}
}

func TestNew_DefaultsAndAMissingScratchRoot(t *testing.T) {
	r, err := New(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxInput(FeatureRender) != 16*mib || r.cfg.ScratchRoot != DefaultScratchRoot() || r.run == nil {
		t.Fatalf("defaults not applied: %+v", r.cfg)
	}
	r.cfg.ScratchRoot = filepath.Join(t.TempDir(), "never-created")
	if n, err := r.SweepScratch(); n != 0 || err != nil {
		t.Fatalf("sweeping a root that was never created: %d, %v", n, err)
	}
	if _, err := New(Config{Timeouts: map[Feature]time.Duration{"x": time.Second}}, nil); err == nil {
		t.Fatal("an unknown timeout key must be refused")
	}
}

func TestRunError_Messages(t *testing.T) {
	for _, tc := range []struct {
		err  *RunError
		want string
	}{
		{&RunError{Outcome: OutcomeTimeout, Feature: FeaturePDF, Detail: "after 1m0s"}, "pdf timed out"},
		{&RunError{Outcome: OutcomeOOM, Feature: FeatureAudio, Detail: "2GiB"}, "audio exceeded its memory limit"},
		{&RunError{Outcome: OutcomeNotAvailable, Feature: FeatureVideo, Detail: "x"}, "video: not available in the agent image"},
		{&RunError{Outcome: OutcomeFailed, Feature: FeatureRender, Detail: "bad"}, "render failed: bad"},
	} {
		if got := tc.err.Error(); !strings.Contains(got, tc.want) {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
	if humanBytes(512*mib) != "512MiB" || humanBytes(2*gib) != "2GiB" || humanBytes(10) != "10B" {
		t.Error("humanBytes")
	}
	if firstLine("a\nb", nil) != "a" || firstLine("", errors.New("e")) != "e" {
		t.Error("firstLine")
	}
}

func removedContainer(f *fakePodman) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c.args) >= 2 && c.args[0] == "rm" {
			return true
		}
	}
	return false
}
