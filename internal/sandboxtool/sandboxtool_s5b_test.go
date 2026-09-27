package sandboxtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/spawn"
)

// S5b (design §7) moves the extractors and voice onto this runner, and the S5a
// code review (review-20260926-076e, NOT GREEN) found seven gaps in it. The
// tests below pin what the runner gained for both.

func ocrSpec() Spec {
	return Spec{
		Feature:    FeatureImageOCR,
		Entrypoint: "tesseract",
		Args:       []string{"/in/image", "/out/ocr"},
		Inputs:     []Input{{Name: "image", Data: []byte("png")}},
	}
}

// piper reads its text only from stdin, so a run can feed one input there.
func TestRun_StdinFeedsTheNamedInput(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, nil)
	res, err := r.Run(context.Background(), Spec{
		Feature: FeatureVoiceTTS, Entrypoint: "piper",
		Args:   []string{"--output_file", "/out/speech.wav"},
		Inputs: []Input{{Name: "text", Data: []byte("hello there")}},
		Stdin:  "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	run := f.runCalls()[0]
	if run.stdin != "hello there" {
		t.Fatalf("stdin = %q", run.stdin)
	}
	if !strings.Contains(strings.Join(run.args, " "), " -i ") {
		t.Fatalf("a run fed on stdin needs -i: %v", run.args)
	}
	if _, err := r.Run(context.Background(), Spec{Feature: FeatureVoiceTTS, Entrypoint: "piper", Stdin: "missing"}); err == nil {
		t.Fatal("stdin naming no input must be refused")
	}
}

// Media too large to hold in memory is copied in from its host path, and
// counts toward the input bound like inline data.
func TestRun_InputPathIsCopiedAndBounded(t *testing.T) {
	src := filepath.Join(t.TempDir(), "clip")
	if err := os.WriteFile(src, []byte("video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen string
	f := &fakePodman{}
	r, err := New(Config{Image: "img", ScratchRoot: t.TempDir()}, func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
		for i, a := range args {
			if host, ok := strings.CutSuffix(a, ":/in:ro,Z"); ok && i > 0 {
				b, _ := os.ReadFile(filepath.Join(host, "video"))
				seen = string(b)
			}
		}
		return f.run(ctx, stdin, name, args...)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Spec{Feature: FeatureVideo, Entrypoint: "ffprobe",
		Inputs: []Input{{Name: "video", Path: src}}})
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	if seen != "video bytes" {
		t.Fatalf("the tool must see the copied input, saw %q", seen)
	}

	r2 := newRunner(t, &fakePodman{}, func(c *Config) { c.MaxInputBytes = 4 })
	if _, err := r2.Run(context.Background(), Spec{Feature: FeatureVideo, Entrypoint: "ffprobe",
		Inputs: []Input{{Name: "video", Path: src}}}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("a path input over the bound must be refused, got %v", err)
	}
	if _, err := r2.Run(context.Background(), Spec{Feature: FeatureVideo, Entrypoint: "ffprobe",
		Inputs: []Input{{Name: "video", Path: filepath.Join(t.TempDir(), "absent")}}}); err == nil {
		t.Fatal("a missing path input must be refused")
	}
}

// The input bound is per feature: a video is not a markdown file.
func TestRun_InputBoundsAreKeyedByFeature(t *testing.T) {
	r := newRunner(t, &fakePodman{}, nil)
	for f, want := range map[Feature]int64{
		FeatureRender: 16 * mib, FeaturePDF: 256 * mib, FeatureImageOCR: 32 * mib,
		FeatureVideo: 2 * gib, FeatureAudio: 512 * mib, FeatureVoiceSTT: 64 * mib, FeatureVoiceTTS: 16 * mib,
	} {
		if got := r.MaxInput(f); got != want {
			t.Errorf("MaxInput(%s) = %d, want %d", f, got, want)
		}
	}
	over := newRunner(t, &fakePodman{}, func(c *Config) { c.MaxInputBytes = 1 << 40 })
	if over.MaxInput(FeatureRender) != 1<<40 {
		t.Error("sandbox_tools.max_input_bytes overrides every feature")
	}
}

// S5a review F3: /out was a writable host directory with no bound.
func TestRun_OversizeOutputIsRefused(t *testing.T) {
	f := &fakePodman{produces: "ocr.txt", size: int(16*mib) + 1}
	r := newRunner(t, f, nil)
	_, err := r.Run(context.Background(), ocrSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeFailed || !strings.Contains(err.Error(), "exceeds the 16MiB limit") {
		t.Fatalf("want an output-size failure, got %v", err)
	}
	if entries, _ := os.ReadDir(r.cfg.ScratchRoot); len(entries) != 0 {
		t.Fatalf("an oversize output must be removed: %v", entries)
	}
}

// §7.1 decision 4: the image declares its tools; a feature whose tool is not
// declared is not available, and no container starts.
func TestRun_TheImageLabelGatesTheSandboxToolFeatures(t *testing.T) {
	onlyPDF := "pdftotext"
	f := &fakePodman{label: &onlyPDF}
	r := newRunner(t, f, nil)
	_, err := r.Run(context.Background(), ocrSpec())
	if !errors.Is(err, ErrNotAvailable) || !strings.Contains(err.Error(), "does not declare tesseract") {
		t.Fatalf("want not available naming tesseract, got %v", err)
	}
	if len(f.runCalls()) != 0 {
		t.Fatal("no container may start for an undeclared tool")
	}
	// render's pandoc predates the label and is not gated by it.
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatalf("render is not label-gated: %v", err)
	}
	res.Close()

	// A rebuilt image that gained the tool is picked up after the refresh
	// interval, without a restart.
	now := time.Now()
	r.now = func() time.Time { return now }
	all := allTools
	f.label = &all
	if _, err := r.Run(context.Background(), ocrSpec()); !errors.Is(err, ErrNotAvailable) {
		t.Fatal("inside the refresh interval the last read stands")
	}
	r.now = func() time.Time { return now.Add(labelRefresh + time.Second) }
	res, err = r.Run(context.Background(), ocrSpec())
	if err != nil {
		t.Fatalf("after the refresh interval the new label applies: %v", err)
	}
	res.Close()
}

func TestLoadTools_OldImagesAndInspectFailures(t *testing.T) {
	none := "<no value>"
	r := newRunner(t, &fakePodman{label: &none}, nil)
	tools, err := r.LoadTools(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatalf("an unlabelled image declares nothing: %v %v", tools, err)
	}
	if _, err := r.Run(context.Background(), ocrSpec()); !errors.Is(err, ErrNotAvailable) ||
		!strings.Contains(err.Error(), "label absent") {
		t.Fatalf("an older image must say why: %v", err)
	}

	r = newRunner(t, &fakePodman{labelErr: errors.New("exit status 125")}, nil)
	if _, err := r.LoadTools(context.Background()); err == nil || !strings.Contains(err.Error(), "image not known") {
		t.Fatalf("an inspect failure must be reported: %v", err)
	}
	if _, err := r.Run(context.Background(), ocrSpec()); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("want not available, got %v", err)
	}

	r = newRunner(t, &fakePodman{}, nil)
	tools, err = r.LoadTools(context.Background())
	if err != nil || len(tools) != 6 || !tools["whisper-cli"] || !tools["piper"] {
		t.Fatalf("the S5b label declares six tools: %v %v", tools, err)
	}
	if ok, detail := r.Declared(context.Background(), "piper"); !ok || detail != "" {
		t.Fatalf("Declared(piper) = %v %q", ok, detail)
	}
	if ok, detail := r.Declared(context.Background(), "pandoc"); ok || !strings.Contains(detail, "does not declare pandoc") {
		t.Fatalf("Declared(pandoc) = %v %q", ok, detail)
	}
	r = newRunner(t, &fakePodman{}, func(c *Config) { c.Image = "" })
	if _, err := r.LoadTools(context.Background()); err == nil {
		t.Fatal("no image configured is an error")
	}
}

// S5a review F1: a daemon killed mid-run leaves its named container; the next
// start removes it, and only its own.
func TestSweepContainers_RemovesThisRunnersLeftoversOnly(t *testing.T) {
	f := &fakePodman{leftovers: "vornik-sbx-aaaa\nvornik-sbx-bbbb\nsomething-else\n"}
	r := newRunner(t, f, nil)
	n, err := r.SweepContainers(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("swept %d (%v), want 2", n, err)
	}
	var listed, removed []string
	for _, c := range f.calls {
		switch c.args[0] {
		case "ps":
			listed = c.args
		case "rm":
			removed = append(removed, c.args[len(c.args)-1])
		}
	}
	if !strings.Contains(strings.Join(listed, " "), "label="+RunLabel+"="+r.scope) {
		t.Fatalf("the sweep must filter on this runner's label: %v", listed)
	}
	if strings.Join(removed, ",") != "vornik-sbx-aaaa,vornik-sbx-bbbb" {
		t.Fatalf("removed %v", removed)
	}
	// Every run carries the label the sweep filters on.
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	if !strings.Contains(strings.Join(f.runCalls()[0].args, " "), "--label "+RunLabel+"="+r.scope) {
		t.Fatal("a run must carry the sweep label")
	}
	other, _ := New(Config{Image: "img", ScratchRoot: t.TempDir()}, f.run)
	if other.Scope() == r.Scope() {
		t.Fatal("two scratch roots (two daemons) must not share a sweep label")
	}
}

// S5a review F2: a failed remove was dropped silently.
func TestRun_ARemoveFailureIsLoggedAndCounted(t *testing.T) {
	var buf bytes.Buffer
	f := &fakePodman{rmErr: errors.New("exit status 125")}
	r := newRunner(t, f, nil)
	r.SetLogger(zerolog.New(&buf))
	r.SetMetrics(NewMetrics(prometheus.NewRegistry()))
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	if !strings.Contains(buf.String(), "could not remove") || !strings.Contains(buf.String(), "vornik-sbx-") {
		t.Fatalf("the failed remove must be logged with the container name: %s", buf.String())
	}
	if got := testutil.ToFloat64(r.metrics.removeFailures); got != 1 {
		t.Fatalf("remove failures = %v, want 1", got)
	}
}

// S5a review F4: a tool at its memory limit that thrashes until the deadline
// was counted as a timeout, hiding the OOM.
func TestRun_AnOOMThatHangsIsCountedAsOOM(t *testing.T) {
	f := &fakePodman{block: make(chan struct{}), oom: true}
	r := newRunner(t, f, func(c *Config) {
		c.Timeouts = map[Feature]time.Duration{FeatureRender: 30 * time.Millisecond}
	})
	_, err := r.Run(context.Background(), renderSpec())
	var re *RunError
	if !errors.As(err, &re) || re.Outcome != OutcomeOOM {
		t.Fatalf("want oom, got %v", err)
	}
}

// S5a review F5: CPU was fixed per feature while the claim said overridable.
func TestRun_CPUsAreOverridablePerFeature(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, func(c *Config) { c.CPUs = map[Feature]string{FeatureRender: "0.5"} })
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	if joined := strings.Join(f.runCalls()[0].args, " "); !strings.Contains(joined, "--cpus=0.5") || !strings.Contains(joined, "OMP_THREAD_LIMIT=1") {
		t.Fatalf("the configured CPU share must apply, and bound OpenMP: %s", joined)
	}
	if threadLimit("2.5") != "3" || threadLimit("x") != "1" || FFmpegThreads != "2" {
		t.Fatal("threadLimit rounds the CPU share up, and falls back to 1")
	}
	if r.CPUs(FeatureVideo) != "2" {
		t.Fatal("an unset feature keeps its default")
	}
	for _, bad := range []map[Feature]string{{FeatureRender: "0"}, {FeatureRender: "x"}, {"nope": "1"}} {
		if _, err := New(Config{CPUs: bad}, nil); err == nil {
			t.Errorf("cpus %v must be refused", bad)
		}
	}
}

// S5a review F6: New's default root lacked the uid suffix production uses.
func TestDefaultScratchRoot_IsPerUser(t *testing.T) {
	if !strings.HasSuffix(DefaultScratchRoot(), fmt.Sprintf("vornik-sandbox-tools-%d", os.Getuid())) {
		t.Fatalf("got %s", DefaultScratchRoot())
	}
}

// S5a review F7: the duration histogram included pool queue wait.
func TestRun_QueueWaitIsMeasuredApartFromRunTime(t *testing.T) {
	f := &fakePodman{}
	r := newRunner(t, f, func(c *Config) { c.MaxConcurrent = 1 })
	r.SetMetrics(NewMetrics(prometheus.NewRegistry()))
	release, err := r.pool.acquire(context.Background(), FeatureRender)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, release)
	res, err := r.Run(context.Background(), renderSpec())
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	wait := histogramSum(t, r.metrics.wait)
	run := histogramSum(t, r.metrics.duration)
	if wait < 0.1 {
		t.Fatalf("wait = %vs, want >= 0.1s", wait)
	}
	if run >= 0.1 {
		t.Fatalf("run duration = %vs must exclude the wait", run)
	}
}

func histogramSum(t *testing.T, h *prometheus.HistogramVec) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(h)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			sum += m.GetHistogram().GetSampleSum()
		}
	}
	return sum
}

// ExecCommand routes through the process-spawn law's kinds (S1b-2): a run of
// any image but the pinned agent image, a flag outside the grammar, or a
// program that is not podman is refused before a process exists.
func TestExecCommand_RefusesWhatTheSpawnLawDoesNotPlace(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"podman", []string{"run", "--network=none", "alpine:latest", "sh"}},
		{"podman", []string{"run", "--privileged", "localhost/vornik-agent:x"}},
		{"podman", []string{"exec", "c", "sh"}},
		{"/bin/sh", []string{"-c", "id"}},
	} {
		if _, err := ExecCommand(ctx, nil, tc.name, tc.args...); !errors.Is(err, spawn.ErrRefused) {
			t.Errorf("%s %v: want spawn.ErrRefused, got %v", tc.name, tc.args, err)
		}
	}
}
