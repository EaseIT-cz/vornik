package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/sandboxtool"
)

// WhisperConfig configures the local whisper.cpp speech-to-text provider.
//
// Both tools it needs run in the pinned agent image through the sandbox
// runner (process-spawn law S5b,
// https://docs.vornik.io §7), never
// on the daemon host: an inbound voice note is untrusted input.
//
//   - ffmpeg, ALWAYS. Inbound audio from Telegram (OGG/Opus) and Slack
//     (MP4/M4A) is normalised to the 16-kHz mono PCM WAV whisper.cpp is
//     built for.
//   - whisper-cli (whisper.cpp), with one ggml-format model file (.bin):
//     ggml-base.en.bin (~150 MB, English-only, fast on CPU) or
//     ggml-medium.bin (~1.5 GB, multilingual, much slower). The model's
//     DIRECTORY is mounted read-only at /models.
//
// Why whisper.cpp vs CGo vs Python (the slice-2 decision): no CGo keeps
// cross-compilation trivial, and no Python runtime keeps the agent image
// free of PyTorch. The same engine serves audio extraction (§7.1 decision 2).
type WhisperConfig struct {
	// ModelPath is the absolute path to the ggml model file
	// (e.g. /var/lib/vornik/voice/ggml-base.en.bin). Required.
	ModelPath string

	// LanguageHint is an optional BCP-47 nudge for the recogniser.
	// Empty leaves whisper.cpp's default (English).
	LanguageHint string

	// Threads pins the thread count. Zero defers to whisper.cpp's
	// default; the sandbox's voice_stt CPU share bounds it either way.
	Threads int

	// Sandbox runs the tools. Nil makes every Transcribe report
	// ErrProviderUnavailable; there is no host fallback.
	Sandbox sandboxtool.Sandbox
}

// whisperLocalSTT runs whisper.cpp as an STTProvider. Each Transcribe is
// two voice_stt sandbox runs, both on the pool's reserved voice slot:
//
//  1. ffmpeg normalises the inbound bytes (in /in) to /out/audio.wav.
//  2. whisper-cli transcribes that WAV with --output-json into /out.
//
// The runner bounds both runs (memory, CPU, timeout) and removes their
// scratch; the caller's ctx cancels them.
type whisperLocalSTT struct {
	cfg WhisperConfig
}

// NewWhisperLocalSTT constructs the provider. Returns an error only when
// the config is structurally broken (empty ModelPath). An absent sandbox or
// a tool the image lacks surfaces as ErrProviderUnavailable on Transcribe.
func NewWhisperLocalSTT(cfg WhisperConfig) (STTProvider, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, errors.New("voice: WhisperConfig.ModelPath is required")
	}
	return &whisperLocalSTT{cfg: cfg}, nil
}

// Transcribe is the STTProvider entry point. See the type comment for the
// two-run flow.
func (w *whisperLocalSTT) Transcribe(ctx context.Context, audio io.Reader, hint Hint) (Transcript, error) {
	if audio == nil {
		return Transcript{}, errors.New("voice: nil audio reader")
	}
	// Read the inbound payload into memory. 64 MiB is a defensive
	// ceiling — the design doc notes Telegram voice messages cap at
	// 1 minute (~120 KiB OGG/Opus) and Slack audio at 5 minutes
	// (~5 MiB MP4/AAC); 64 MiB is comfortable headroom, and the
	// sandbox's voice_stt input bound.
	const maxInboundBytes = 64 * 1024 * 1024
	audioBytes, err := io.ReadAll(io.LimitReader(audio, maxInboundBytes+1))
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: read inbound audio: %w", err)
	}
	if int64(len(audioBytes)) > maxInboundBytes {
		return Transcript{}, fmt.Errorf("voice: inbound audio exceeds %d byte cap", maxInboundBytes)
	}
	if len(audioBytes) == 0 {
		return Transcript{}, errors.New("voice: empty audio input")
	}
	if w.cfg.Sandbox == nil {
		return Transcript{}, fmt.Errorf("%w: %w (no sandbox runner)", ErrProviderUnavailable, sandboxtool.ErrNotAvailable)
	}
	if err := modelReady("voice.stt.model", w.cfg.ModelPath); err != nil {
		return Transcript{}, err
	}

	// Run 1: ffmpeg normalise to 16 kHz mono 16-bit PCM WAV.
	norm, err := w.cfg.Sandbox.Run(ctx, NormaliseSpec(audioBytes))
	if err != nil {
		return Transcript{}, runFailure("ffmpeg normalise", err)
	}
	defer norm.Close()
	wavPath := filepath.Join(norm.OutDir, "audio.wav")
	if st, serr := os.Stat(wavPath); serr != nil || st.Size() == 0 {
		return Transcript{}, errors.New("voice: ffmpeg produced empty WAV")
	}

	// Run 2: whisper-cli. -oj -of writes /out/transcript.json; -np keeps
	// the combined output a diagnostic.
	args := []string{"-m", "/models/" + filepath.Base(w.cfg.ModelPath), "-f", "/in/audio.wav",
		"-oj", "-of", "/out/transcript", "-np"}
	if w.cfg.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(w.cfg.Threads))
	}
	lang := strings.TrimSpace(hint.LanguageHint)
	if lang == "" {
		lang = strings.TrimSpace(w.cfg.LanguageHint)
	}
	if code := languageCode(lang); code != "" {
		args = append(args, "-l", code)
	}
	res, err := w.cfg.Sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureVoiceSTT,
		Entrypoint: "whisper-cli",
		Args:       args,
		Inputs:     []sandboxtool.Input{{Name: "audio.wav", Path: wavPath}},
		ModelDir:   filepath.Dir(w.cfg.ModelPath),
	})
	if err != nil {
		return Transcript{}, runFailure("whisper.cpp", err)
	}
	defer res.Close()
	rawJSON, err := os.ReadFile(filepath.Join(res.OutDir, "transcript.json"))
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: read whisper JSON: %w", err)
	}
	return parseWhisperJSON(rawJSON)
}

// NormaliseSpec is the voice_stt run that decodes an inbound voice note
// (Telegram OGG/Opus, Slack MP4/AAC — the container is detected from the
// header) into /out/audio.wav, 16 kHz mono 16-bit PCM. Exported so `vornikctl
// doctor` decodes its Opus and AAC samples with exactly this run.
func NormaliseSpec(audio []byte) sandboxtool.Spec {
	return sandboxtool.Spec{
		Feature:    sandboxtool.FeatureVoiceSTT,
		Entrypoint: "ffmpeg",
		Args: []string{"-nostdin", "-loglevel", "error", "-threads", sandboxtool.FFmpegThreads, "-filter_threads", sandboxtool.FFmpegThreads,
			"-i", "/in/voice", "-threads", sandboxtool.FFmpegThreads, "-ac", "1", "-ar", "16000", "-acodec", "pcm_s16le", "-f", "wav", "/out/audio.wav"},
		Inputs: []sandboxtool.Input{{Name: "voice", Data: audio}},
	}
}

// languageCode maps a BCP-47 hint ("en-US") to the short code whisper.cpp
// takes ("en"). Anything that is not two or three letters is dropped, so a
// hint can never reach argv as anything but a language code.
func languageCode(hint string) string {
	short := hint
	if idx := strings.IndexAny(short, "-_"); idx > 0 {
		short = short[:idx]
	}
	short = strings.ToLower(short)
	if len(short) < 2 || len(short) > 3 {
		return ""
	}
	for _, r := range short {
		if r < 'a' || r > 'z' {
			return ""
		}
	}
	return short
}

// runFailure wraps a sandbox run's error; a tool the image lacks (or no
// image) is ErrProviderUnavailable as well, so the channel adapters keep
// their "voice is not set up" handling.
// modelReady pre-flights a voice model on every call, as the audio extractor
// does: a model removed or replaced by a directory after startup would
// otherwise fail as a podman mount error naming neither the setting nor the
// file, and start a container for nothing (S5b review residual R4).
func modelReady(setting, path string) error {
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return fmt.Errorf("%w: model %s is not a readable file (check %s)", ErrProviderUnavailable, path, setting)
	}
	return nil
}

func runFailure(step string, err error) error {
	if errors.Is(err, sandboxtool.ErrNotAvailable) {
		return fmt.Errorf("%w: %s: %w", ErrProviderUnavailable, step, err)
	}
	return fmt.Errorf("voice: %s failed: %w", step, err)
}

// whisperJSONShape mirrors the subset of whisper.cpp's --output-json
// envelope we consume: the language verdict, and the per-segment text,
// offsets and confidence (when present).
//
// Layout (abbreviated):
//
//	{
//	  "result": { "language": "en" },
//	  "params": { "model": "ggml-base.en.bin", "language": "en" },
//	  "transcription": [
//	    { "text": "...", "offsets": { "from": 0, "to": 5000 } },
//	    ...
//	  ]
//	}
//
// Builds that emit a "confidence" or "no_speech_prob" are honoured
// (treating 1 - no_speech_prob as the segment confidence); otherwise the
// confidence is 0 (unknown).
type whisperJSONShape struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Params struct {
		Language string `json:"language"`
	} `json:"params"`
	Transcription []struct {
		Text    string `json:"text"`
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		NoSpeechProb *float64 `json:"no_speech_prob,omitempty"`
		Confidence   *float64 `json:"confidence,omitempty"`
	} `json:"transcription"`
}

// parseWhisperJSON folds the JSON into a Transcript. Combines the
// per-segment texts (space-separated), takes the max-To offset as the
// duration, and averages the per-segment confidence (when any segment
// reported one).
func parseWhisperJSON(raw []byte) (Transcript, error) {
	var doc whisperJSONShape
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Transcript{}, fmt.Errorf("voice: whisper JSON parse: %w", err)
	}
	parts := make([]string, 0, len(doc.Transcription))
	var totalConf float64
	var confSamples int
	var maxTo int64
	for _, seg := range doc.Transcription {
		txt := strings.TrimSpace(seg.Text)
		if txt != "" {
			parts = append(parts, txt)
		}
		if seg.Offsets.To > maxTo {
			maxTo = seg.Offsets.To
		}
		switch {
		case seg.Confidence != nil:
			totalConf += *seg.Confidence
			confSamples++
		case seg.NoSpeechProb != nil:
			totalConf += 1.0 - *seg.NoSpeechProb
			confSamples++
		}
	}
	out := Transcript{
		Text:       strings.Join(parts, " "),
		DurationMs: maxTo,
	}
	if doc.Result.Language != "" {
		out.Language = doc.Result.Language
	} else {
		out.Language = doc.Params.Language
	}
	if confSamples > 0 {
		out.Confidence = totalConf / float64(confSamples)
	}
	return out, nil
}

// Compile-time guard: whisperLocalSTT satisfies STTProvider.
var _ STTProvider = (*whisperLocalSTT)(nil)
