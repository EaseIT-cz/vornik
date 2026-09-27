// Tests for the video extractor. ffprobe and ffmpeg run in the agent image
// through the sandbox runner (process-spawn law S5b); here a fake sandbox
// plays them — the contract under test is the run shape, the JSON parsing,
// the sampling plan, and the manifest's honesty, none of which need a real
// codec. The real tools run under the podman e2e lane.
package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// fakeBins plays ffprobe (writing ffprobeJSON to the -o path) and ffmpeg
// (writing frameCount JPEG-ish frames) in a fake sandbox.
func fakeBins(t *testing.T, ffprobeJSON string, frameCount int) *sandboxtest.Fake {
	t.Helper()
	return sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		switch spec.Entrypoint {
		case "ffprobe":
			return os.WriteFile(filepath.Join(out, "probe.json"), []byte(ffprobeJSON), 0o600)
		case "ffmpeg":
			for i := 1; i <= frameCount; i++ {
				name := fmt.Sprintf("frame-%03d.jpg", i)
				if err := os.WriteFile(filepath.Join(out, name), []byte(fmt.Sprintf("JPEGDATA%d", i)), 0o600); err != nil {
					return err
				}
			}
			return nil
		}
		return fmt.Errorf("unexpected tool %s", spec.Entrypoint)
	})
}

const probeJSON = `{
  "format": {"duration": "120.5", "format_name": "mov,mp4,m4a"},
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080, "r_frame_rate": "30000/1001"},
    {"codec_type": "audio", "codec_name": "aac"}
  ]
}`

func srcFile(t *testing.T) extractor.Source {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(p, []byte("fake video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return extractor.Source{FilePath: p, MimeType: "video/mp4", OriginalName: "clip.mp4"}
}

func TestExtract_MetadataAndFrames(t *testing.T) {
	bins := fakeBins(t, probeJSON, 4)
	e := NewWithOptions(bins, 4, 5)

	res, err := e.Extract(context.Background(), srcFile(t))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Title != "clip" {
		t.Errorf("title = %q, want clip", res.Metadata.Title)
	}
	if res.Metadata.DurationSeconds != 120 {
		t.Errorf("duration = %d, want 120", res.Metadata.DurationSeconds)
	}
	for k, want := range map[string]string{
		"width": "1920", "height": "1080", "video_codec": "h264",
		"audio_codec": "aac", "has_audio_track": "true", "frames_sampled": "4",
	} {
		if got := res.Metadata.Extra[k]; got != want {
			t.Errorf("Extra[%q] = %q, want %q", k, got, want)
		}
	}
	// fps from the rational 30000/1001.
	if fps := res.Metadata.Extra["fps"]; !strings.HasPrefix(fps, "29.97") {
		t.Errorf("fps = %q, want ~29.97", fps)
	}
	if len(res.Sections) != 2 || len(res.Outline) != 2 {
		t.Fatalf("want 2 sections + 2 outline entries, got %d/%d", len(res.Sections), len(res.Outline))
	}
	// Frames become ProducedFiles so the Runner can persist them; a temp dir
	// would be gone before anything could look at them.
	if len(res.Files) != 4 {
		t.Fatalf("want 4 produced frames, got %d", len(res.Files))
	}
	if res.Files[0].RelPath != "frame-001.jpg" || len(res.Files[0].Content) == 0 {
		t.Errorf("unexpected first frame: %+v", res.Files[0])
	}
}

// The manifest must state the limits of uniform sampling. Without this an
// agent answers "what happens at the end" from the last SAMPLED frame as
// though it were the last frame — the same confident-but-unfounded shape as
// describing an image nobody looked at.
func TestExtract_FrameManifestStatesItsLimits(t *testing.T) {
	bins := fakeBins(t, probeJSON, 3)
	e := NewWithOptions(bins, 3, 10)

	res, err := e.Extract(context.Background(), srcFile(t))
	if err != nil {
		t.Fatal(err)
	}
	manifest := res.Sections[1].Content
	for _, want := range []string{
		"uniform",
		"may appear in no frame",
		"Do not\ndescribe the video's ending from the last sampled frame",
		"do not state\nthat something never appears merely because no frame shows it",
		"frames do not carry visual timing",
		"files/frame-001.jpg",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %q:\n%s", want, manifest)
		}
	}
}

// Frame timestamps must line up with the interval actually used, or the
// manifest's own claims are wrong.
func TestExtract_FrameOffsetsMatchInterval(t *testing.T) {
	bins := fakeBins(t, probeJSON, 3)
	e := NewWithOptions(bins, 3, 40)

	res, err := e.Extract(context.Background(), srcFile(t))
	if err != nil {
		t.Fatal(err)
	}
	manifest := res.Sections[1].Content
	// 120s / 3 frames = 40s interval → offsets 0s, 40s, 1m20s.
	for _, want := range []string{"approximately 0s", "approximately 40s", "approximately 1m20s"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing offset %q:\n%s", want, manifest)
		}
	}
}

// Frame sampling failing must not fail the extraction: metadata an operator
// can see beats a failed task that explains nothing. But the manifest must
// say the visual content is unestablished rather than staying silent.
func TestExtract_FrameFailureDegradesHonestly(t *testing.T) {
	bins := fakeBins(t, probeJSON, 0) // ffmpeg writes nothing
	e := NewWithOptions(bins, 4, 5)

	res, err := e.Extract(context.Background(), srcFile(t))
	if err != nil {
		t.Fatalf("a frame failure must not fail the extraction: %v", err)
	}
	if res.Metadata.Extra["frames_sampled"] != "0" {
		t.Errorf("frames_sampled = %q", res.Metadata.Extra["frames_sampled"])
	}
	if res.Metadata.Extra["frame_sampling_error"] == "" {
		t.Error("the failure must be recorded in metadata, not swallowed")
	}
	if !strings.Contains(res.Sections[1].Content, "Nothing about the video's visual content has been established") {
		t.Errorf("manifest must state the absence plainly:\n%s", res.Sections[1].Content)
	}
	if len(res.Files) != 0 {
		t.Errorf("no frames means no produced files, got %d", len(res.Files))
	}
}

// Both runs are the fixed video shape: feature video, the input copied from
// its path under a fixed name, outputs written to /out, no user text in argv.
func TestExtract_RunsProbeAndFramesInTheSandbox(t *testing.T) {
	bins := fakeBins(t, probeJSON, 2)
	src := srcFile(t)
	if _, err := NewWithOptions(bins, 2, 5).Extract(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	specs := bins.Specs()
	if len(specs) != 2 {
		t.Fatalf("want a probe run and a frames run, got %d", len(specs))
	}
	probe, frames := specs[0], specs[1]
	if probe.Feature != sandboxtool.FeatureVideo || probe.Entrypoint != "ffprobe" ||
		strings.Join(probe.Args, " ") != "-v error -print_format json -show_format -show_streams -o /out/probe.json /in/video" {
		t.Fatalf("probe run = %+v", probe)
	}
	// Sampling selects the first frame, then one per interval. It used to be
	// fps=1/<interval>, which emits NO frame from a clip shorter than the
	// interval: the S5b e2e lane's 2 s clip sampled nothing (2026-09-26).
	if frames.Feature != sandboxtool.FeatureVideo || frames.Entrypoint != "ffmpeg" ||
		strings.Join(frames.Args, " ") != `-nostdin -loglevel error -threads 2 -filter_threads 2 -i /in/video -threads 2 -vf select=isnan(prev_selected_t)+gte(t-prev_selected_t\,60) -fps_mode vfr -frames:v 2 -q:v 3 /out/frame-%03d.jpg` {
		t.Fatalf("frames run = %+v", frames)
	}
	for _, s := range specs {
		if len(s.Inputs) != 1 || s.Inputs[0].Name != "video" || s.Inputs[0].Path != src.FilePath {
			t.Fatalf("inputs = %+v", s.Inputs)
		}
	}
}

// §7.1 decision 4: no sandbox, or an image without ffprobe, is "not
// available" — never a host fallback.
func TestExtract_NotAvailableNeverFallsBackToTheHost(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, tool := range []string{"ffprobe", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := New(nil).Extract(context.Background(), srcFile(t)); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: want not available, got %v", err)
	}
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return sandboxtest.NotAvailable(sandboxtool.FeatureVideo)
	})
	if _, err := New(sb).Extract(context.Background(), srcFile(t)); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("undeclared tool: want not available, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a host ffprobe/ffmpeg ran")
	}
}

func TestExtract_EmptyPathRejected(t *testing.T) {
	if _, err := New(nil).Extract(context.Background(), extractor.Source{}); err == nil {
		t.Fatal("an empty file path must be rejected")
	}
}

func TestExtract_NoAudioTrackSaysSo(t *testing.T) {
	silent := `{"format":{"duration":"30.0"},"streams":[{"codec_type":"video","codec_name":"vp9","width":640,"height":480,"r_frame_rate":"25/1"}]}`
	bins := fakeBins(t, silent, 2)
	e := NewWithOptions(bins, 2, 5)

	res, err := e.Extract(context.Background(), srcFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Metadata.Extra["has_audio_track"]; ok {
		t.Error("a silent video must not claim an audio track")
	}
	if !strings.Contains(res.Sections[0].Content, "no audio track") {
		t.Errorf("metadata section should state the absence:\n%s", res.Sections[0].Content)
	}
}

func TestExtract_BadProbeJSON(t *testing.T) {
	bins := fakeBins(t, `{not json`, 1)
	e := NewWithOptions(bins, 2, 5)
	if _, err := e.Extract(context.Background(), srcFile(t)); err == nil {
		t.Fatal("malformed ffprobe output must error")
	}
}

func TestSamplingPlan(t *testing.T) {
	e := NewWithOptions(nil, 8, 5)
	// A long video: interval scales so the frame cap is respected.
	interval, frames := e.samplingPlan(800)
	if frames > 8 {
		t.Errorf("frame cap breached: %d", frames)
	}
	if interval < 5 {
		t.Errorf("interval floor breached: %d", interval)
	}
	// A short clip: the floor wins, so we don't emit 8 near-identical frames.
	interval, frames = e.samplingPlan(12)
	if interval != 5 {
		t.Errorf("short clip interval = %d, want the 5s floor", interval)
	}
	if frames > 8 || frames < 1 {
		t.Errorf("short clip frames = %d", frames)
	}
	// Unknown duration falls back to floor interval + cap.
	interval, frames = e.samplingPlan(0)
	if interval != 5 || frames != 8 {
		t.Errorf("unknown duration plan = %d/%d, want 5/8", interval, frames)
	}
	// Defaults apply when unset.
	d := New(nil)
	if i, f := d.samplingPlan(0); i != defaultMinIntervalSeconds || f != defaultMaxFrames {
		t.Errorf("default plan = %d/%d", i, f)
	}
}

func TestParseFrameRateAndSeconds(t *testing.T) {
	if got := parseFrameRate("30000/1001"); got < 29.9 || got > 30.0 {
		t.Errorf("parseFrameRate = %v", got)
	}
	for _, bad := range []string{"", "30", "x/y", "30/0"} {
		if got := parseFrameRate(bad); got != 0 {
			t.Errorf("parseFrameRate(%q) = %v, want 0", bad, got)
		}
	}
	if got := parseSeconds(" 12.5 "); got != 12.5 {
		t.Errorf("parseSeconds = %v", got)
	}
	for _, bad := range []string{"", "abc", "-1"} {
		if got := parseSeconds(bad); got != 0 {
			t.Errorf("parseSeconds(%q) = %v, want 0", bad, got)
		}
	}
}

func TestFormatOffset(t *testing.T) {
	for in, want := range map[int]string{0: "0s", 45: "45s", 60: "1m00s", 125: "2m05s"} {
		if got := formatOffset(in); got != want {
			t.Errorf("formatOffset(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleFromSource(t *testing.T) {
	if got := titleFromSource(extractor.Source{OriginalName: "Holiday Clip.mp4"}); got != "Holiday Clip" {
		t.Errorf("got %q", got)
	}
	if got := titleFromSource(extractor.Source{FilePath: "/tmp/x/movie.mkv"}); got != "movie" {
		t.Errorf("got %q", got)
	}
	if got := titleFromSource(extractor.Source{FilePath: "/tmp/x/.mkv"}); got != "video" {
		t.Errorf("extensionless fallback = %q, want video", got)
	}
}

func TestNameAndVersion(t *testing.T) {
	e := New(nil)
	if e.Name() != "vornik-extract-video" || e.Version() == "" {
		t.Errorf("identity wrong: %s %s", e.Name(), e.Version())
	}
}

// Duration is the probe run Extract makes, exported for `vornikctl doctor`.
func TestDuration_IsTheProbeRun(t *testing.T) {
	bins := fakeBins(t, probeJSON, 0)
	secs, hasAudio, err := New(bins).Duration(context.Background(), srcFile(t).FilePath)
	if err != nil || secs != 120.5 || !hasAudio {
		t.Fatalf("Duration = %v %v %v", secs, hasAudio, err)
	}
	if len(bins.Specs()) != 1 || bins.Specs()[0].Entrypoint != "ffprobe" {
		t.Fatalf("runs = %+v", bins.Specs())
	}
	if _, _, err := New(nil).Duration(context.Background(), "/x"); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: %v", err)
	}
	if _, _, err := New(fakeBins(t, "{bad", 0)).Duration(context.Background(), srcFile(t).FilePath); err == nil {
		t.Fatal("bad JSON must error")
	}
}
