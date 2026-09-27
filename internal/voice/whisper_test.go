package voice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// whisper.cpp and its ffmpeg normalise run in the agent image through the
// sandbox runner (process-spawn law S5b, design §7); a fake sandbox plays
// them here, and the real tools run under the podman e2e lane.

const helloJSON = `{
	"result": {"language": "en"},
	"params": {"language": "en"},
	"transcription": [
		{"text": " Hello, world.", "offsets": {"from": 0, "to": 1200}, "no_speech_prob": 0.05}
	]
}`

// whisperTools plays ffmpeg (writing /out/audio.wav) and whisper-cli
// (writing /out/transcript.json). Either may be replaced by an error.
type whisperTools struct {
	wav        []byte
	ffmpegErr  error
	transcript string
	whisperErr error
}

func (w whisperTools) fake(t *testing.T) *sandboxtest.Fake {
	return sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		switch spec.Entrypoint {
		case "ffmpeg":
			if w.ffmpegErr != nil {
				return w.ffmpegErr
			}
			if w.wav == nil {
				return nil
			}
			return os.WriteFile(filepath.Join(out, "audio.wav"), w.wav, 0o600)
		case "whisper-cli":
			if w.whisperErr != nil {
				return w.whisperErr
			}
			if w.transcript == "" {
				return nil
			}
			return os.WriteFile(filepath.Join(out, "transcript.json"), []byte(w.transcript), 0o600)
		}
		return errors.New("unexpected tool " + spec.Entrypoint)
	})
}

func newWhisper(t *testing.T, sb sandboxtool.Sandbox, cfg WhisperConfig) STTProvider {
	t.Helper()
	if cfg.ModelPath == "" {
		cfg.ModelPath = "ggml-base.en.bin"
	}
	cfg.ModelPath = modelFile(t, cfg.ModelPath)
	cfg.Sandbox = sb
	prov, err := NewWhisperLocalSTT(cfg)
	if err != nil {
		t.Fatalf("NewWhisperLocalSTT: %v", err)
	}
	return prov
}

func TestWhisperLocalSTT_New_RequiresModelPath(t *testing.T) {
	if _, err := NewWhisperLocalSTT(WhisperConfig{}); err == nil {
		t.Fatal("expected error when ModelPath is empty")
	}
}

func TestWhisperLocalSTT_Transcribe_NilReader(t *testing.T) {
	w := newWhisper(t, whisperTools{}.fake(t), WhisperConfig{})
	if _, err := w.Transcribe(context.Background(), nil, Hint{}); err == nil {
		t.Error("expected error on nil reader")
	}
}

func TestWhisperLocalSTT_Transcribe_EmptyAudio(t *testing.T) {
	w := newWhisper(t, whisperTools{}.fake(t), WhisperConfig{})
	if _, err := w.Transcribe(context.Background(), bytes.NewReader(nil), Hint{}); err == nil {
		t.Error("expected error on empty audio")
	}
}

func TestWhisperLocalSTT_Transcribe_OversizeAudio(t *testing.T) {
	w := newWhisper(t, whisperTools{}.fake(t), WhisperConfig{})
	big := bytes.NewReader(make([]byte, 64*1024*1024+1))
	_, err := w.Transcribe(context.Background(), big, Hint{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want oversize-audio error", err)
	}
}

// Telegram's OGG/Opus and Slack's MP4/AAC voice notes: ffmpeg normalises
// them to 16 kHz mono WAV, then whisper-cli transcribes — two voice_stt
// runs, the model mounted from its DIRECTORY, no user text in argv.
func TestWhisperLocalSTT_Transcribe_HappyPath(t *testing.T) {
	sb := whisperTools{wav: makeWAV(16000, 16000), transcript: helloJSON}.fake(t)
	w := newWhisper(t, sb, WhisperConfig{ModelPath: "/var/lib/vornik/voice/ggml-base.en.bin", Threads: 4})
	tr, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("OggS-mock-payload")), Hint{LanguageHint: "en-US", MimeType: "audio/ogg"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "Hello, world." || tr.Language != "en" || tr.DurationMs != 1200 {
		t.Errorf("transcript = %+v", tr)
	}
	if tr.Confidence < 0.94 || tr.Confidence > 0.96 {
		t.Errorf("Confidence = %v, want ~0.95", tr.Confidence)
	}
	specs := sb.Specs()
	if len(specs) != 2 {
		t.Fatalf("want ffmpeg then whisper-cli, got %d runs", len(specs))
	}
	norm, whisper := specs[0], specs[1]
	if norm.Feature != sandboxtool.FeatureVoiceSTT || norm.Entrypoint != "ffmpeg" ||
		strings.Join(norm.Args, " ") != "-nostdin -loglevel error -threads 2 -filter_threads 2 -i /in/voice -threads 2 -ac 1 -ar 16000 -acodec pcm_s16le -f wav /out/audio.wav" ||
		len(norm.Inputs) != 1 || norm.Inputs[0].Name != "voice" || string(norm.Inputs[0].Data) != "OggS-mock-payload" {
		t.Fatalf("normalise run = %+v", norm)
	}
	if whisper.Feature != sandboxtool.FeatureVoiceSTT || whisper.Entrypoint != "whisper-cli" ||
		whisper.ModelDir != filepath.Dir(w.(*whisperLocalSTT).cfg.ModelPath) ||
		strings.Join(whisper.Args, " ") != "-m /models/ggml-base.en.bin -f /in/audio.wav -oj -of /out/transcript -np -t 4 -l en" {
		t.Fatalf("whisper run = %+v", whisper)
	}
	if len(whisper.Inputs) != 1 || whisper.Inputs[0].Name != "audio.wav" {
		t.Fatalf("whisper input = %+v", whisper.Inputs)
	}
}

// A language hint reaches argv only as a short language code: anything else
// is dropped, so a hint can never smuggle a flag.
func TestWhisperLocalSTT_LanguageHintIsACodeOrNothing(t *testing.T) {
	for hint, want := range map[string]string{
		"de-DE": "-l de", "EN": "-l en", "": "", "--model /etc/passwd": "", "e": "", "english": "",
	} {
		sb := whisperTools{wav: makeWAV(16000, 100), transcript: helloJSON}.fake(t)
		w := newWhisper(t, sb, WhisperConfig{})
		if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{LanguageHint: hint}); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(sb.Specs()[1].Args, " ")
		if want == "" && strings.Contains(args, " -l ") {
			t.Errorf("hint %q must not reach argv: %s", hint, args)
		}
		if want != "" && !strings.HasSuffix(args, want) {
			t.Errorf("hint %q: argv %s, want suffix %q", hint, args, want)
		}
	}
	// The configured hint applies when the message carries none.
	sb := whisperTools{wav: makeWAV(16000, 100), transcript: helloJSON}.fake(t)
	w := newWhisper(t, sb, WhisperConfig{LanguageHint: "cs"})
	if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{}); err != nil {
		t.Fatal(err)
	}
	if !sliceContains(sb.Specs()[1].Args, "-l", "cs") {
		t.Errorf("configured hint: %v", sb.Specs()[1].Args)
	}
}

// §7.1 decision 4: no sandbox, or an image without the tools, reports the
// provider unavailable and never runs a host whisper or ffmpeg.
func TestWhisperLocalSTT_NotAvailableNeverRunsTheHost(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, tool := range []string{"whisper-cli", "whisper-cpp", "main", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	w := newWhisper(t, nil, WhisperConfig{})
	if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{}); !errors.Is(err, ErrProviderUnavailable) || !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: err = %v", err)
	}
	w = newWhisper(t, whisperTools{ffmpegErr: sandboxtest.NotAvailable(sandboxtool.FeatureVoiceSTT)}.fake(t), WhisperConfig{})
	if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("undeclared ffmpeg: err = %v", err)
	}
	w = newWhisper(t, whisperTools{wav: makeWAV(16000, 100), whisperErr: sandboxtest.NotAvailable(sandboxtool.FeatureVoiceSTT)}.fake(t), WhisperConfig{})
	if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("undeclared whisper-cli: err = %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a host whisper/ffmpeg ran")
	}
}

func TestWhisperLocalSTT_Transcribe_ToolFailuresSurface(t *testing.T) {
	cases := map[string]whisperTools{
		"ffmpeg fails":      {ffmpegErr: sandboxtest.Failed(sandboxtool.FeatureVoiceSTT, "Invalid data found when processing input")},
		"ffmpeg no output":  {},
		"whisper fails":     {wav: makeWAV(16000, 100), whisperErr: sandboxtest.Failed(sandboxtool.FeatureVoiceSTT, "failed to initialize whisper context")},
		"no transcript":     {wav: makeWAV(16000, 100)},
		"malformed JSON":    {wav: makeWAV(16000, 100), transcript: "{not json"},
		"empty normalising": {wav: []byte{}},
	}
	for name, tools := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWhisper(t, tools.fake(t), WhisperConfig{})
			if _, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	w := newWhisper(t, cases["ffmpeg fails"].fake(t), WhisperConfig{})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("x")), Hint{})
	if !strings.Contains(err.Error(), "Invalid data found") {
		t.Errorf("the tool's message must surface: %v", err)
	}
}

func TestParseWhisperJSON_MultiSegment(t *testing.T) {
	body := `{
		"result": {"language": "de"},
		"params": {"language": ""},
		"transcription": [
			{"text": " Guten ", "offsets": {"from": 0, "to": 500}, "no_speech_prob": 0.1},
			{"text": "Tag.", "offsets": {"from": 500, "to": 1500}, "no_speech_prob": 0.2}
		]
	}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Text != "Guten Tag." {
		t.Errorf("Text = %q, want %q", tr.Text, "Guten Tag.")
	}
	if tr.Language != "de" {
		t.Errorf("Language = %q, want de", tr.Language)
	}
	if tr.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", tr.DurationMs)
	}
	// confidence avg = (0.9 + 0.8) / 2 = 0.85
	if tr.Confidence < 0.84 || tr.Confidence > 0.86 {
		t.Errorf("Confidence = %v, want ~0.85", tr.Confidence)
	}
}

func TestParseWhisperJSON_ConfidenceExplicit(t *testing.T) {
	body := `{
		"result": {"language": "en"},
		"transcription": [
			{"text": "Hi", "offsets": {"from": 0, "to": 300}, "confidence": 0.42}
		]
	}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Confidence < 0.41 || tr.Confidence > 0.43 {
		t.Errorf("Confidence = %v, want ~0.42", tr.Confidence)
	}
}

func TestParseWhisperJSON_NoConfidenceReturnsZero(t *testing.T) {
	body := `{"transcription":[{"text":"Hi","offsets":{"from":0,"to":100}}]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0 (no signal)", tr.Confidence)
	}
}

func TestParseWhisperJSON_EmptyTranscription(t *testing.T) {
	body := `{"result":{"language":"en"},"transcription":[]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Text != "" {
		t.Errorf("Text = %q, want empty (silent audio)", tr.Text)
	}
	if tr.Language != "en" {
		t.Errorf("Language = %q, want en (from result)", tr.Language)
	}
}

func TestParseWhisperJSON_FallbackToParamsLanguage(t *testing.T) {
	body := `{"params":{"language":"fr"},"transcription":[{"text":"Bonjour","offsets":{"from":0,"to":600}}]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Language != "fr" {
		t.Errorf("Language = %q, want fr (from params fallback)", tr.Language)
	}
}

func TestParseWhisperJSON_BadJSON(t *testing.T) {
	if _, err := parseWhisperJSON([]byte("not-json")); err == nil {
		t.Error("expected error on bad JSON")
	}
}
