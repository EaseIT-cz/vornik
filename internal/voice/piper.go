package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/sandboxtool"
)

// PiperConfig configures the local Piper text-to-speech provider.
//
// Both tools it needs run in the pinned agent image through the sandbox
// runner (process-spawn law S5b,
// https://docs.vornik.io §7), never
// on the daemon host:
//
//   - piper (2023.11.14-2 in the image), with one voice model: the .onnx
//     and its .onnx.json side by side. The model's DIRECTORY is mounted
//     read-only at /models, so piper finds the .json beside the .onnx.
//   - ffmpeg, when callers ask for a Format other than "wav": Piper emits
//     WAV; the transcode turns it into ogg-opus for Telegram or mp4-aac
//     for Slack.
//
// A missing sandbox or a tool the image lacks surfaces as
// ErrProviderUnavailable on the first Synthesize call, NOT at
// construction, so the daemon boots where voice is opt-in.
type PiperConfig struct {
	// ModelPath is the absolute path to the voice model's .onnx file.
	// Required — Piper has no implicit default model.
	ModelPath string

	// DefaultVoice is the fallback when TTSOptions.VoiceID is empty.
	// Piper's CLI doesn't actually use a voice name — the voice IS
	// the .onnx model — but we keep the field for symmetry with
	// hosted providers (slice 7) and to surface it in audit logs.
	DefaultVoice string

	// DefaultSpeed is the fallback when TTSOptions.Speed is 0.
	// Piper's --length_scale takes the INVERSE of speed (length 0.5 =
	// 2x faster); the provider does that translation.
	DefaultSpeed float64

	// MaxTextRunes caps one synthesis call. Defends against an LLM
	// reply that's wider than the platform's 1-minute / 5-minute
	// voice envelope. Zero falls back to defaultPiperMaxRunes
	// (1500 runes ~ 90 seconds at conversational pace).
	MaxTextRunes int

	// Sandbox runs the tools. Nil makes every Synthesize report
	// ErrProviderUnavailable; there is no host fallback.
	Sandbox sandboxtool.Sandbox
}

const (
	defaultPiperVoice    = "en_US-amy-medium"
	defaultPiperSpeed    = 1.0
	defaultPiperMaxRunes = 1500
)

// piperLocalTTS runs Piper as a TTSProvider. Each Synthesize is one or two
// voice_tts sandbox runs, on the pool's reserved voice slot:
//
//  1. piper reads the text on stdin (from a file in /in, so the text is
//     never argv) and writes /out/speech.wav.
//  2. when Format is not "wav", ffmpeg transcodes that WAV into /out.
type piperLocalTTS struct {
	cfg PiperConfig
}

// NewPiperLocalTTS constructs the provider. Returns an error only when the
// config is structurally broken (empty ModelPath).
func NewPiperLocalTTS(cfg PiperConfig) (TTSProvider, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, errors.New("voice: PiperConfig.ModelPath is required")
	}
	if cfg.DefaultVoice == "" {
		cfg.DefaultVoice = defaultPiperVoice
	}
	if cfg.DefaultSpeed <= 0 {
		cfg.DefaultSpeed = defaultPiperSpeed
	}
	if cfg.MaxTextRunes <= 0 {
		cfg.MaxTextRunes = defaultPiperMaxRunes
	}
	return &piperLocalTTS{cfg: cfg}, nil
}

// Synthesize is the TTSProvider entry point. Validates inputs, runs piper
// in the sandbox, transcodes if needed, and returns the encoded audio +
// metadata. Honors ctx cancellation.
func (p *piperLocalTTS) Synthesize(ctx context.Context, text string, opts TTSOptions) (Audio, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Audio{}, ErrEmptyText
	}
	if runesIn(trimmed) > p.cfg.MaxTextRunes {
		return Audio{}, fmt.Errorf("%w: %d runes > %d cap",
			ErrOversizeText, runesIn(trimmed), p.cfg.MaxTextRunes)
	}
	if p.cfg.Sandbox == nil {
		return Audio{}, fmt.Errorf("%w: %w (no sandbox runner)", ErrProviderUnavailable, sandboxtool.ErrNotAvailable)
	}
	if err := modelReady("voice.tts.voice", p.cfg.ModelPath); err != nil {
		return Audio{}, err
	}

	speed := opts.Speed
	if speed <= 0 {
		speed = p.cfg.DefaultSpeed
	}
	// Piper's --length_scale is the inverse of speed: shorter scale =
	// faster speech.
	lengthScale := 1.0 / speed

	res, err := p.cfg.Sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureVoiceTTS,
		Entrypoint: "piper",
		Args: []string{
			"--model", "/models/" + filepath.Base(p.cfg.ModelPath),
			"--length_scale", formatFloat(lengthScale),
			"--output_file", "/out/speech.wav",
			"--quiet",
		},
		Inputs:   []sandboxtool.Input{{Name: "text", Data: []byte(trimmed)}},
		Stdin:    "text",
		ModelDir: filepath.Dir(p.cfg.ModelPath),
	})
	if err != nil {
		// piper's own message ("Unable to load voice", ...) is in the
		// run error's detail.
		return Audio{}, runFailure("piper", err)
	}
	defer res.Close()
	wavPath := filepath.Join(res.OutDir, "speech.wav")
	wav, err := os.ReadFile(wavPath)
	if err != nil || len(wav) == 0 {
		return Audio{}, errors.New("voice: piper produced empty output")
	}

	// Piper writes a RIFF WAV header followed by PCM samples. The
	// parser only consults the header to set SampleRateHz and
	// DurationMs; the bytes pass through unchanged to the transcode
	// step (or the caller, when Format=="wav").
	sampleRate, durationMs, parseErr := parseWAV(wav)
	if parseErr != nil {
		return Audio{}, fmt.Errorf("voice: piper output not a parseable WAV: %w", parseErr)
	}

	format := opts.Format
	if format == "" {
		format = "wav"
	}
	mime := map[string]string{"wav": "audio/wav", "ogg-opus": "audio/ogg", "mp4-aac": "audio/mp4"}[format]
	if mime == "" {
		return Audio{}, fmt.Errorf("voice: unsupported format %q (want wav | ogg-opus | mp4-aac)", format)
	}
	out := wav
	if format != "wav" {
		if out, err = p.transcode(ctx, wavPath, format); err != nil {
			return Audio{}, err
		}
	}
	return Audio{
		Bytes:        out,
		MimeType:     mime,
		DurationMs:   durationMs,
		SampleRateHz: sampleRate,
	}, nil
}

// transcode runs ffmpeg in the sandbox to convert Piper's WAV to either
// ogg-opus (Telegram-native) or mp4-aac (Slack-native). Driven by a canned
// arg list per target format so the call site stays small.
func (p *piperLocalTTS) transcode(ctx context.Context, wavPath, format string) ([]byte, error) {
	spec, output, err := TranscodeSpec(wavPath, format)
	if err != nil {
		return nil, err
	}
	res, err := p.cfg.Sandbox.Run(ctx, spec)
	if err != nil {
		return nil, runFailure("ffmpeg transcode", err)
	}
	defer res.Close()
	encoded, err := os.ReadFile(filepath.Join(res.OutDir, output))
	if err != nil || len(encoded) == 0 {
		return nil, errors.New("voice: ffmpeg transcode produced empty output")
	}
	return encoded, nil
}

// TranscodeSpec is the voice_tts run that encodes the WAV at wavPath for a
// channel: "ogg-opus" (Telegram) or "mp4-aac" (Slack). It returns the run
// and the name of the file it writes in /out. Exported so `vornikctl doctor`
// encodes its Opus and AAC samples with exactly this run.
func TranscodeSpec(wavPath, format string) (sandboxtool.Spec, string, error) {
	var args []string
	var output string
	switch format {
	case "ogg-opus":
		// Telegram's sendVoice expects OGG with a single Opus stream.
		// 48 kHz is the only Opus-native rate; ffmpeg resamples
		// Piper's 22.05 kHz output. -application voip biases the
		// encoder for speech latency over music fidelity.
		output = "/out/reply.ogg"
		args = []string{"-nostdin", "-loglevel", "error", "-threads", sandboxtool.FFmpegThreads, "-filter_threads", sandboxtool.FFmpegThreads,
			"-f", "wav", "-i", "/in/speech.wav", "-threads", sandboxtool.FFmpegThreads,
			"-c:a", "libopus", "-b:a", "32k", "-application", "voip", "-ar", "48000", "-ac", "1",
			"-f", "ogg", output}
	case "mp4-aac":
		// Slack's audio clip UI renders MP4/AAC inline. The fragmented
		// layout is kept from the stdout era: players accept it and it
		// streams.
		output = "/out/reply.m4a"
		args = []string{"-nostdin", "-loglevel", "error", "-threads", sandboxtool.FFmpegThreads, "-filter_threads", sandboxtool.FFmpegThreads,
			"-f", "wav", "-i", "/in/speech.wav", "-threads", sandboxtool.FFmpegThreads,
			"-c:a", "aac", "-b:a", "64k", "-ar", "44100", "-ac", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", output}
	default:
		return sandboxtool.Spec{}, "", fmt.Errorf("voice: transcode: unsupported format %q", format)
	}
	return sandboxtool.Spec{
		Feature:    sandboxtool.FeatureVoiceTTS,
		Entrypoint: "ffmpeg",
		Args:       args,
		Inputs:     []sandboxtool.Input{{Name: "speech.wav", Path: wavPath}},
	}, filepath.Base(output), nil
}

// parseWAV reads the minimal RIFF/WAVE header to extract sample rate
// and duration. Doesn't validate every chunk — we trust Piper to emit
// a sane file and only need the two numbers for downstream metadata.
// The parser is defensive against truncated headers; returns an error
// when the file is too short to be a WAV at all.
//
// Wire layout (little-endian):
//
//	offset 0:  "RIFF"
//	offset 8:  "WAVE"
//	offset 12: "fmt "
//	offset 16: chunk size (4 bytes)
//	offset 20: audio format (2 bytes, 1 = PCM)
//	offset 22: num channels (2 bytes)
//	offset 24: sample rate (4 bytes)
//	offset 28: byte rate (4 bytes)
//	...
//	offset 36: "data"
//	offset 40: data chunk size (4 bytes)
//	offset 44: PCM samples
func parseWAV(b []byte) (sampleRate int, durationMs int64, err error) {
	if len(b) < 44 {
		return 0, 0, fmt.Errorf("wav too short: %d bytes", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, errors.New("not a RIFF/WAVE file")
	}
	// numChannels = b[22:24] LE
	channels := int(binary.LittleEndian.Uint16(b[22:24]))
	if channels <= 0 {
		channels = 1
	}
	sampleRate = int(binary.LittleEndian.Uint32(b[24:28]))
	byteRate := int(binary.LittleEndian.Uint32(b[28:32]))
	dataSize := int(binary.LittleEndian.Uint32(b[40:44]))
	if byteRate <= 0 {
		// Fall back to (sampleRate * channels * 16-bit). Piper always
		// emits 16-bit PCM; we don't read the bits-per-sample field
		// explicitly because the byte rate already encodes it.
		byteRate = sampleRate * channels * 2
	}
	if byteRate <= 0 {
		return sampleRate, 0, nil
	}
	durationMs = int64(dataSize) * 1000 / int64(byteRate)
	return sampleRate, durationMs, nil
}

// runesIn counts UTF-8 rune length without iterating twice.
func runesIn(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// formatFloat renders a float as a short decimal for CLI args. Avoids
// the scientific-notation forms strconv.FormatFloat emits for small
// values (Piper's flag parser doesn't accept "1e-2").
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// Compile-time guard: piperLocalTTS satisfies TTSProvider.
var _ TTSProvider = (*piperLocalTTS)(nil)
