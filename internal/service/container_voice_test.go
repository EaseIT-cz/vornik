package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/voice"
)

// TestBuildSTTProvider_EmptyProviderReturnsNil — operator hasn't
// wired STT; helper returns (nil, nil) so initVoice leaves
// c.voiceSTT nil and the channel adapters stay on the
// text-only path.
func TestBuildSTTProvider_EmptyProviderReturnsNil(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{}, nil)
	if err != nil {
		t.Fatalf("buildSTTProvider(empty): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildSTTProvider_UnknownProviderReturnsNil — an unsupported
// provider name parses without error but yields a nil provider;
// initVoice surfaces this via a warn-level log rather than failing
// boot, since voice is opt-in scaffolding.
func TestBuildSTTProvider_UnknownProviderReturnsNil(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{Provider: "deepgram-cloud"}, nil)
	if err != nil {
		t.Fatalf("buildSTTProvider(unknown): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider for unknown name, got %T", p)
	}
}

// TestBuildSTTProvider_WhisperLocalRequiresModel — the well-formed-
// provider path bubbles up provider-side validation errors so the
// operator sees the typo loudly at boot.
func TestBuildSTTProvider_WhisperLocalRequiresModel(t *testing.T) {
	_, err := buildSTTProvider(config.VoiceSTTConfig{Provider: "whisper-local"}, nil)
	if err == nil {
		t.Fatal("expected error from whisper-local with empty model, got nil")
	}
	if !strings.Contains(err.Error(), "ModelPath") {
		t.Errorf("error %q should mention ModelPath", err.Error())
	}
}

// TestBuildSTTProvider_WhisperLocalHappyPath — well-formed config
// constructs a non-nil provider. Missing binaries surface at
// Transcribe time, not here.
func TestBuildSTTProvider_WhisperLocalHappyPath(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{
		Provider: "whisper-local",
		Model:    "/tmp/fake.ggml.bin",
	}, nil)
	if err != nil {
		t.Fatalf("buildSTTProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestBuildSTTProvider_CaseInsensitiveProviderName — guards
// against operator-side capitalisation typos in YAML.
func TestBuildSTTProvider_CaseInsensitiveProviderName(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{
		Provider: "Whisper-Local",
		Model:    "/tmp/fake.ggml.bin",
	}, nil)
	if err != nil {
		t.Fatalf("buildSTTProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestBuildTTSProvider_EmptyProviderReturnsNil — symmetric to STT.
func TestBuildTTSProvider_EmptyProviderReturnsNil(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{}, nil)
	if err != nil {
		t.Fatalf("buildTTSProvider(empty): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildTTSProvider_UnknownProviderReturnsNil — unsupported
// names parse without error.
func TestBuildTTSProvider_UnknownProviderReturnsNil(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{Provider: "elevenlabs"}, nil)
	if err != nil {
		t.Fatalf("buildTTSProvider(unknown): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildTTSProvider_PiperRequiresVoice — provider validation
// errors surface to the operator at boot.
func TestBuildTTSProvider_PiperRequiresVoice(t *testing.T) {
	_, err := buildTTSProvider(config.VoiceTTSConfig{Provider: "piper"}, nil)
	if err == nil {
		t.Fatal("expected error from piper with empty voice, got nil")
	}
	if !strings.Contains(err.Error(), "ModelPath") {
		t.Errorf("error %q should mention ModelPath", err.Error())
	}
}

// TestBuildTTSProvider_PiperHappyPath — well-formed config returns
// a non-nil provider.
func TestBuildTTSProvider_PiperHappyPath(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{
		Provider:     "piper",
		Voice:        "/tmp/voice.onnx",
		Speed:        1.0,
		MaxTextRunes: 1500,
	}, nil)
	if err != nil {
		t.Fatalf("buildTTSProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestInitVoice_EmptyConfigSucceedsAndLeavesBothNil — daemon boots
// without a voice block; initVoice succeeds, both providers stay
// nil, and the channel adapters fall back to their text-only paths.
func TestInitVoice_EmptyConfigSucceedsAndLeavesBothNil(t *testing.T) {
	c := &Container{
		Config: &config.Config{},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT != nil {
		t.Errorf("voiceSTT should be nil, got %T", c.voiceSTT)
	}
	if c.voiceTTS != nil {
		t.Errorf("voiceTTS should be nil, got %T", c.voiceTTS)
	}
}

// TestInitVoice_UnsupportedProviderLogsAndLeavesNil — unknown
// provider names don't fail the daemon; the warn-log is the
// signal the operator sees.
func TestInitVoice_UnsupportedProviderLogsAndLeavesNil(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "deepgram-cloud"},
				TTS: config.VoiceTTSConfig{Provider: "elevenlabs"},
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT != nil {
		t.Errorf("voiceSTT should be nil for unsupported provider, got %T", c.voiceSTT)
	}
	if c.voiceTTS != nil {
		t.Errorf("voiceTTS should be nil for unsupported provider, got %T", c.voiceTTS)
	}
}

// TestInitVoice_MalformedSupportedProviderFailsBoot — a supported
// provider with a structurally broken config (no model path) is
// loud-fail at boot, not silent.
func TestInitVoice_MalformedSupportedProviderFailsBoot(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "whisper-local"}, // missing Model
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err == nil {
		t.Fatal("expected error for whisper-local without model, got nil")
	}
}

// TestInitVoice_HappyPathWiresBoth — well-formed STT + TTS land on
// the container so downstream channel adapters can pick them up.
func TestInitVoice_HappyPathWiresBoth(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "whisper-local", Model: "/tmp/m.bin"},
				TTS: config.VoiceTTSConfig{Provider: "piper", Voice: "/tmp/v.onnx"},
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT == nil {
		t.Error("voiceSTT should be set")
	}
	if c.voiceTTS == nil {
		t.Error("voiceTTS should be set")
	}
}

// captureLogger returns a zerolog.Logger that writes JSON records
// into the supplied buffer so tests can assert on the log surface
// produced by initVoice's probes.
func captureLogger(buf *bytes.Buffer) zerolog.Logger {
	return zerolog.New(buf)
}

// logRecords parses the captured log buffer (one JSON object per
// line) into a slice of decoded maps. Test helper for asserting on
// the diagnostic surface.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findLog returns the first log record whose "message" field
// contains the substring needle; nil when no match.
func findLog(records []map[string]any, needle string) map[string]any {
	for _, r := range records {
		if msg, ok := r["message"].(string); ok && strings.Contains(msg, needle) {
			return r
		}
	}
	return nil
}

// TestProbeModel_MissingFile — operator config points at a model
// file that isn't there; warn so they download it.
func TestProbeModel_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", "/does/not/exist/ggml-base.en.bin")

	rec := findLog(logRecords(t, &buf), "model file not found")
	if rec == nil {
		t.Fatalf("expected 'model file not found' warning; got %q", buf.String())
	}
	if rec["level"] != "warn" {
		t.Errorf("level = %v, want warn", rec["level"])
	}
}

// TestProbeModel_HappyPath — model file exists; INFO with the
// size so the operator can sanity-check the download didn't get
// truncated.
func TestProbeModel_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	model := filepath.Join(tmp, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("ggml-fake-bytes"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", model)

	rec := findLog(logRecords(t, &buf), "model OK")
	if rec == nil {
		t.Fatalf("expected 'model OK' info; got %q", buf.String())
	}
	if got, want := int64(rec["size_bytes"].(float64)), int64(len("ggml-fake-bytes")); got != want {
		t.Errorf("size_bytes = %d, want %d", got, want)
	}
}

// TestProbeModel_IsDirectory — common config typo: operator
// points at a directory containing the model instead of the file
// itself. Surface a distinct warning.
func TestProbeModel_IsDirectory(t *testing.T) {
	tmp := t.TempDir()
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", tmp)

	rec := findLog(logRecords(t, &buf), "is a directory, not a file")
	if rec == nil {
		t.Fatalf("expected 'is a directory' warning; got %q", buf.String())
	}
}

// TestProbeModel_EmptyPath — defensive: nil config snuck through
// to the probe. Should warn rather than crash.
func TestProbeModel_EmptyPath(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", "")

	rec := findLog(logRecords(t, &buf), "model path is empty")
	if rec == nil {
		t.Fatalf("expected 'model path is empty' warning; got %q", buf.String())
	}
}

// TestInitVoice_LogsResolvedConfigForSTT — when the operator wires STT,
// the boot-time config dump exposes the model path, and the model is
// checked on the host (a stat, no spawn: the tools run in the agent image).
func TestInitVoice_LogsResolvedConfigForSTT(t *testing.T) {
	tmp := t.TempDir()
	model := filepath.Join(tmp, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("data"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	var buf bytes.Buffer
	c := &Container{
		Logger: captureLogger(&buf),
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "whisper-local", Model: model},
			},
		},
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	records := logRecords(t, &buf)
	for _, want := range []string{"configuring STT provider", "whisper model OK", "voice providers initialized"} {
		if findLog(records, want) == nil {
			t.Errorf("missing %q; got %q", want, buf.String())
		}
	}
	if findLog(records, "ignored") != nil {
		t.Errorf("nothing is ignored when no host path is set: %q", buf.String())
	}
}

// TestInitVoice_WarnsOnEachMisconfig — a model file that is missing, and
// the host binary paths that no longer do anything (process-spawn law S5b:
// whisper-cli, piper and ffmpeg run in the agent image), each get a WARN.
func TestInitVoice_WarnsOnEachMisconfig(t *testing.T) {
	voiceDir := t.TempDir()
	onnx := filepath.Join(voiceDir, "en_US-lessac-low.onnx")
	if err := os.WriteFile(onnx, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	c := &Container{
		Logger: captureLogger(&buf),
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{
					Provider:   "whisper-local",
					Model:      "/no/such/model.bin",
					BinaryPath: "/no/such/whisper-cli",
					FFmpegPath: "/no/such/ffmpeg",
				},
				TTS: config.VoiceTTSConfig{Provider: "piper", Voice: onnx, BinaryPath: "/usr/bin/piper"},
			},
		},
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	records := logRecords(t, &buf)
	for _, want := range []string{
		"whisper model file not found",
		"voice.stt.binary_path is ignored",
		"voice.stt.ffmpeg_path is ignored",
		"voice.tts.binary_path is ignored",
		"piper voice config not found beside the model",
	} {
		if findLog(records, want) == nil {
			t.Errorf("missing warning %q; got %q", want, buf.String())
		}
	}
}

// Voice runs through the daemon's sandbox runner: initVoice hands it to both
// providers, so a Transcribe with no runner reports unavailable rather than
// reaching for a host binary.
func TestInitVoice_ProvidersUseTheSandbox(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{Voice: config.VoiceConfig{
			STT: config.VoiceSTTConfig{Provider: "whisper-local", Model: "/tmp/m.bin"},
			TTS: config.VoiceTTSConfig{Provider: "piper", Voice: "/tmp/v.onnx"},
		}},
	}
	if err := c.initVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.voiceSTT.Transcribe(context.Background(), strings.NewReader("x"), voice.Hint{}); !errors.Is(err, voice.ErrProviderUnavailable) {
		t.Fatalf("no runner: %v", err)
	}
	if _, err := c.voiceTTS.Synthesize(context.Background(), "hi", voice.TTSOptions{}); !errors.Is(err, voice.ErrProviderUnavailable) {
		t.Fatalf("no runner: %v", err)
	}
	if c.sandbox() != nil {
		t.Fatal("a nil runner must be a nil Sandbox, not a typed nil")
	}
}
