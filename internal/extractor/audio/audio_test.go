// Tests for the audio extractor. ffmpeg and whisper-cli (whisper.cpp) run in
// the agent image through the sandbox runner (process-spawn law S5b, design
// §7.1 decision 2); a fake sandbox plays them here and the real tools run
// under the podman e2e lane. The rest is unit-tested via the parsing seams.
package audio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// whisperCppJSON is the shape whisper-cli -oj writes (whisper.cpp v1.8.7).
const whisperCppJSON = `{
  "result": {"language": "en"},
  "transcription": [
    {"timestamps": {"from": "00:00:00,000", "to": "00:00:05,000"}, "offsets": {"from": 0, "to": 5000}, "text": " Hello, world."},
    {"timestamps": {"from": "00:00:05,000", "to": "00:00:08,000"}, "offsets": {"from": 5000, "to": 8000}, "text": " [BLANK_AUDIO]"},
    {"timestamps": {"from": "00:00:08,000", "to": "00:01:02,500"}, "offsets": {"from": 8000, "to": 62500}, "text": " Second sentence here."}
  ]
}`

func writeModel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(path, []byte("ggml"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeAudio(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meeting notes.mp3")
	if err := os.WriteFile(path, []byte("ID3 audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeTools plays ffmpeg (normalising to /out/audio.wav) and whisper-cli
// (writing its JSON to /out/transcript.json).
func fakeTools(t *testing.T, transcript string) *sandboxtest.Fake {
	return sandboxtest.New(t, func(spec sandboxtool.Spec, in map[string][]byte, out string) error {
		switch spec.Entrypoint {
		case "ffmpeg":
			return os.WriteFile(filepath.Join(out, "audio.wav"), []byte("RIFF wav of "+string(in["audio"])), 0o600)
		case "whisper-cli":
			return os.WriteFile(filepath.Join(out, "transcript.json"), []byte(transcript), 0o600)
		}
		return errors.New("unexpected tool " + spec.Entrypoint)
	})
}

// Design §7.1 decision 2: the audio extractor moves from the Python whisper
// CLI to whisper.cpp in the sandbox — ffmpeg normalises, whisper-cli
// transcribes with the model mounted from its DIRECTORY at /models.
func TestExtract_NormalisesThenTranscribesInTheSandbox(t *testing.T) {
	model := writeModel(t)
	audioPath := writeAudio(t)
	sb := fakeTools(t, whisperCppJSON)
	res, err := New(sb, model).Extract(context.Background(), extractor.Source{
		FilePath: audioPath, MimeType: "audio/mpeg", OriginalName: "standup.mp3",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	specs := sb.Specs()
	if len(specs) != 2 {
		t.Fatalf("want an ffmpeg run and a whisper-cli run, got %d", len(specs))
	}
	norm, whisper := specs[0], specs[1]
	if norm.Feature != sandboxtool.FeatureAudio || norm.Entrypoint != "ffmpeg" ||
		strings.Join(norm.Args, " ") != "-nostdin -loglevel error -threads 2 -filter_threads 2 -i /in/audio -threads 2 -ac 1 -ar 16000 -acodec pcm_s16le -f wav /out/audio.wav" ||
		len(norm.Inputs) != 1 || norm.Inputs[0].Name != "audio" || norm.Inputs[0].Path != audioPath {
		t.Fatalf("normalise run = %+v", norm)
	}
	if whisper.Feature != sandboxtool.FeatureAudio || whisper.Entrypoint != "whisper-cli" ||
		strings.Join(whisper.Args, " ") != "-m /models/ggml-base.en.bin -f /in/audio.wav -l auto -oj -of /out/transcript -np" ||
		whisper.ModelDir != filepath.Dir(model) {
		t.Fatalf("whisper run = %+v", whisper)
	}
	if len(whisper.Inputs) != 1 || whisper.Inputs[0].Name != "audio.wav" {
		t.Fatalf("whisper input = %+v", whisper.Inputs)
	}
	// Non-speech markers ([BLANK_AUDIO]) are dropped; timestamps come from
	// the millisecond offsets.
	if len(res.Sections) != 2 || res.Sections[1].Content != "Second sentence here." {
		t.Fatalf("sections = %+v", res.Sections)
	}
	if res.Outline[1].TimestampStartSec != 8 || res.Sections[1].Title != "00:00:08 — 00:01:02" {
		t.Fatalf("timestamps: %+v %+v", res.Outline[1], res.Sections[1])
	}
	if res.Metadata.Language != "en" || res.Metadata.DurationSeconds != 62 || res.Metadata.Title != "standup" {
		t.Fatalf("metadata = %+v", res.Metadata)
	}
}

// Design §7.1 decision 3: with no model configured (neither
// extractors.audio.model_path nor voice.stt.model), audio extraction is
// "not available", and no tool runs.
func TestExtract_NoModelIsNotAvailable(t *testing.T) {
	for name, model := range map[string]string{
		"unset":   "",
		"missing": filepath.Join(t.TempDir(), "absent.bin"),
	} {
		t.Run(name, func(t *testing.T) {
			sb := fakeTools(t, whisperCppJSON)
			_, err := New(sb, model).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)})
			if !errors.Is(err, sandboxtool.ErrNotAvailable) {
				t.Fatalf("want not available, got %v", err)
			}
			if !strings.Contains(err.Error(), "extractors.audio.model_path") {
				t.Fatalf("the error must name the key to set: %v", err)
			}
			if len(sb.Specs()) != 0 {
				t.Fatal("no tool may run without a model")
			}
		})
	}
}

// §7.1 decision 4: never a host fallback — neither to whisper-cli nor to the
// Python whisper CLI this extractor used to run.
func TestExtract_NotAvailableNeverFallsBackToTheHost(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, tool := range []string{"whisper", "whisper-cli", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := New(nil, writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)}); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: want not available, got %v", err)
	}
	sb := sandboxtest.New(t, func(sandboxtool.Spec, map[string][]byte, string) error {
		return sandboxtest.NotAvailable(sandboxtool.FeatureAudio)
	})
	if _, err := New(sb, writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)}); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("undeclared tool: want not available, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a host whisper/ffmpeg ran")
	}
}

func TestExtract_SilenceIsNoSpeech(t *testing.T) {
	silent := `{"result":{"language":"en"},"transcription":[{"offsets":{"from":0,"to":1000},"text":" [BLANK_AUDIO]"}]}`
	_, err := New(fakeTools(t, silent), writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)})
	if !errors.Is(err, ErrNoSpeech) {
		t.Fatalf("want ErrNoSpeech, got %v", err)
	}
}

func TestExtract_ToolFailuresSurface(t *testing.T) {
	sb := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, _ string) error {
		if spec.Entrypoint == "ffmpeg" {
			return sandboxtest.Failed(sandboxtool.FeatureAudio, "Invalid data found when processing input")
		}
		return nil
	})
	_, err := New(sb, writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)})
	if err == nil || !strings.Contains(err.Error(), "Invalid data found") {
		t.Fatalf("the tool's message must surface: %v", err)
	}
	if _, err := New(fakeTools(t, "{not json"), writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)}); err == nil {
		t.Fatal("malformed whisper JSON must error")
	}
	noJSON := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		if spec.Entrypoint == "ffmpeg" {
			return os.WriteFile(filepath.Join(out, "audio.wav"), []byte("RIFF"), 0o600)
		}
		return nil
	})
	if _, err := New(noJSON, writeModel(t)).Extract(context.Background(), extractor.Source{FilePath: writeAudio(t)}); err == nil {
		t.Fatal("a missing transcript must error")
	}
}

func TestExtract_EmptyPath_Errors(t *testing.T) {
	_, err := New(nil, "").Extract(context.Background(), extractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

// TestBuildSections_FiltersBadSegments — whisper sometimes emits
// near-empty segments + segments dominated by silence detection
// (no_speech_prob high). Verify the section builder drops both
// while preserving the rest in reading order.
func TestBuildSections_FiltersBadSegments(t *testing.T) {
	segments := []whisperSegment{
		{ID: 0, Start: 0, End: 5, Text: "Hello, world.", NoSpeechProb: 0.05},
		{ID: 1, Start: 5, End: 10, Text: "  ", NoSpeechProb: 0.10}, // empty after trim
		{ID: 2, Start: 10, End: 15, Text: "Second sentence here.", NoSpeechProb: 0.05},
		{ID: 3, Start: 15, End: 20, Text: "uhh", NoSpeechProb: 0.92}, // no-speech dominated
		{ID: 4, Start: 20, End: 30, Text: "Continuing the discussion.", NoSpeechProb: 0.02},
	}
	sections, outline := buildSections(segments)
	if len(sections) != 3 {
		t.Fatalf("sections = %d; want 3 (rejecting blank + no-speech)", len(sections))
	}
	if len(outline) != len(sections) {
		t.Errorf("outline (%d) != sections (%d)", len(outline), len(sections))
	}
	if sections[0].Content != "Hello, world." {
		t.Errorf("first section content = %q", sections[0].Content)
	}
	if sections[2].Content != "Continuing the discussion." {
		t.Errorf("last section content = %q", sections[2].Content)
	}
	if outline[0].TimestampStartSec != 0 {
		t.Errorf("first segment timestamp = %d; want 0", outline[0].TimestampStartSec)
	}
	if outline[2].TimestampStartSec != 20 {
		t.Errorf("third segment timestamp = %d; want 20", outline[2].TimestampStartSec)
	}
}

func TestBuildSections_AllEmpty_ReturnsNothing(t *testing.T) {
	segments := []whisperSegment{
		{Start: 0, End: 5, Text: "   ", NoSpeechProb: 0.05},
		{Start: 5, End: 10, Text: "", NoSpeechProb: 0.10},
	}
	sections, _ := buildSections(segments)
	if len(sections) != 0 {
		t.Errorf("blank-segment-only input must yield zero sections; got %d", len(sections))
	}
}

func TestFormatTimestamp(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "00:00:00"},
		{12, "00:00:12"},
		{125, "00:02:05"},
		{3661, "01:01:01"},
		{-5, "00:00:00"}, // negative defends against floating-point noise from whisper
	}
	for _, c := range cases {
		got := formatTimestamp(c.in)
		if got != c.want {
			t.Errorf("formatTimestamp(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestFormatTimestampRange(t *testing.T) {
	got := formatTimestampRange(125.3, 158.7)
	if got != "00:02:05 — 00:02:38" {
		t.Errorf("formatTimestampRange = %q", got)
	}
}

func TestTitleFromSource_StripsExtension(t *testing.T) {
	cases := map[string]string{
		"voice-memo.mp3": "voice-memo",
		"clip.m4a":       "clip",
		"recording":      "recording", // no extension preserved
		"":               "",
	}
	for in, want := range cases {
		got := titleFromSource(extractor.Source{OriginalName: in})
		if got != want {
			t.Errorf("titleFromSource(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestDurationFromSegments(t *testing.T) {
	if got := durationFromSegments(nil); got != 0 {
		t.Errorf("nil → 0; got %v", got)
	}
	segs := []whisperSegment{
		{End: 30}, {End: 75.5}, {End: 180.2},
	}
	if got := durationFromSegments(segs); got != 180.2 {
		t.Errorf("durationFromSegments = %v; want 180.2", got)
	}
}

func TestExtractor_Identifies(t *testing.T) {
	e := New(nil, "")
	if e.Name() != Name {
		t.Errorf("Name = %q; want %q", e.Name(), Name)
	}
	if e.Version() != Version {
		t.Errorf("Version = %q; want %q", e.Version(), Version)
	}
}
