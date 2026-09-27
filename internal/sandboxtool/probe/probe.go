// Package probe runs each sandbox tool once on a generated fixture and checks
// what it does, not only that it starts (process-spawn law S5b, design §7.1
// decision 4, S5-F10): pdftotext and tesseract on a one-page fixture, ffmpeg
// encoding and decoding the Opus and AAC that Telegram and Slack voice need,
// ffprobe reading a clip's duration, whisper-cli transcribing a one-second
// clip with EACH effective model, and piper synthesising a short phrase with
// the configured voice.
//
// Every probe goes through the same code the daemon uses — the extractors and
// voice providers on a sandboxtool runner — so it runs with the same flags
// and limits, and a model path that is unset, missing or the wrong family
// fails here rather than at the first voice note. `vornikctl doctor` runs it
// (internal/hostdoctor); the podman e2e lane runs it against the real image.
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/audio"
	imagex "vornik.io/vornik/internal/extractor/image"
	"vornik.io/vornik/internal/extractor/pdf"
	"vornik.io/vornik/internal/extractor/video"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/voice"
)

// Models are the effective model paths: voice.stt.model,
// the audio extractor's model (extractors.audio.model_path, else
// voice.stt.model), and voice.tts.voice. Empty skips the probe.
type Models struct {
	VoiceSTT        string
	AudioExtraction string
	VoiceTTS        string
}

// Status is a probe's verdict.
type Status string

// The verdicts. NotAvailable means the image (or the runner) lacks the tool:
// the feature reports so visibly; Failed means the tool ran and did the wrong
// thing, or a configured model is unusable.
const (
	StatusOK           Status = "OK"
	StatusFailed       Status = "FAILED"
	StatusNotAvailable Status = "NOT AVAILABLE"
	StatusSkipped      Status = "SKIPPED"
)

// Result is one probe's verdict.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Run runs every probe through sb, in order, and never stops at a failure.
func Run(ctx context.Context, sb sandboxtool.Sandbox, m Models) []Result {
	dir, err := os.MkdirTemp("", "vornik-probe-*")
	if err != nil {
		return []Result{{Name: "fixtures", Status: StatusFailed, Detail: err.Error()}}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	f := fixtures{dir: dir}

	results := []Result{
		probePDF(ctx, sb, f),
		probeOCR(ctx, sb, f),
		probeCodec(ctx, sb, f, "opus", "ogg-opus"),
		probeCodec(ctx, sb, f, "aac", "mp4-aac"),
		probeDuration(ctx, sb, f),
		probeVoiceSTT(ctx, sb, m.VoiceSTT),
		probeAudioExtraction(ctx, sb, f, m),
		probePiper(ctx, sb, m.VoiceTTS),
	}
	return results
}

type fixtures struct{ dir string }

func (f fixtures) write(name string, data []byte) (string, error) {
	path := filepath.Join(f.dir, name)
	return path, os.WriteFile(path, data, 0o600)
}

func verdict(name string, err error, okDetail string) Result {
	switch {
	case err == nil:
		return Result{Name: name, Status: StatusOK, Detail: okDetail}
	case errors.Is(err, sandboxtool.ErrNotAvailable):
		return Result{Name: name, Status: StatusNotAvailable, Detail: err.Error()}
	default:
		return Result{Name: name, Status: StatusFailed, Detail: err.Error()}
	}
}

func probePDF(ctx context.Context, sb sandboxtool.Sandbox, f fixtures) Result {
	path, err := f.write("probe.pdf", PDF(PDFText))
	if err == nil {
		var res extractor.Result
		if res, err = pdf.New(sb).Extract(ctx, extractor.Source{FilePath: path}); err == nil &&
			(len(res.Sections) == 0 || !strings.Contains(res.Sections[0].Content, PDFText)) {
			err = fmt.Errorf("pdftotext did not return the fixture's text %q", PDFText)
		}
	}
	return verdict("pdftotext", err, "extracted the text of a one-page PDF")
}

func probeOCR(ctx context.Context, sb sandboxtool.Sandbox, f fixtures) Result {
	path, err := f.write("probe.png", TextPNG(OCRWord, 5))
	if err != nil {
		return verdict("tesseract", err, "")
	}
	res, err := imagex.New(sb).Extract(ctx, extractor.Source{FilePath: path})
	if err == nil {
		// The image extractor degrades OCR to a footer; the doctor wants
		// the verdict itself.
		content := ""
		if len(res.Sections) > 0 {
			content = res.Sections[0].Content
		}
		switch engine := res.Metadata.Extra["ocr_engine"]; {
		case strings.HasPrefix(engine, "none"):
			err = fmt.Errorf("tesseract: %w (%s)", sandboxtool.ErrNotAvailable, footer(content))
		case engine != "tesseract":
			err = fmt.Errorf("tesseract failed: %s", footer(content))
		case !strings.Contains(strings.ToUpper(content), OCRWord):
			err = fmt.Errorf("tesseract did not read the fixture's word %q: %s", OCRWord, footer(content))
		}
	}
	return verdict("tesseract", err, "recognised a rendered word")
}

// footer is the last line of an image section: where the extractor says
// what OCR did.
func footer(content string) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	return lines[len(lines)-1]
}

// probeCodec encodes the sine fixture as a voice reply would be (the
// voice_tts transcode) and decodes it as an inbound voice note would be (the
// voice_stt normalise): the Opus and AAC paths Telegram and Slack need.
func probeCodec(ctx context.Context, sb sandboxtool.Sandbox, f fixtures, name, format string) Result {
	name = "ffmpeg " + name
	if sb == nil {
		return verdict(name, fmt.Errorf("ffmpeg: %w (no sandbox runner)", sandboxtool.ErrNotAvailable), "")
	}
	wav, err := f.write("probe.wav", SineWAV(1, 16000))
	if err != nil {
		return verdict(name, err, "")
	}
	spec, output, err := voice.TranscodeSpec(wav, format)
	if err != nil {
		return verdict(name, err, "")
	}
	enc, err := sb.Run(ctx, spec)
	if err != nil {
		return verdict(name, fmt.Errorf("encode: %w", err), "")
	}
	encoded, rerr := os.ReadFile(filepath.Join(enc.OutDir, output))
	enc.Close()
	if rerr != nil || len(encoded) == 0 {
		return verdict(name, fmt.Errorf("encode produced no %s", format), "")
	}
	dec, err := sb.Run(ctx, voice.NormaliseSpec(encoded))
	if err != nil {
		return verdict(name, fmt.Errorf("decode: %w", err), "")
	}
	defer dec.Close()
	decoded, rerr := os.ReadFile(filepath.Join(dec.OutDir, "audio.wav"))
	if rerr != nil || len(decoded) <= 44 || !bytes.HasPrefix(decoded, []byte("RIFF")) {
		return verdict(name, fmt.Errorf("decode produced no audio from %s", format), "")
	}
	return verdict(name, nil, "encoded and decoded "+format)
}

func probeDuration(ctx context.Context, sb sandboxtool.Sandbox, f fixtures) Result {
	path, err := f.write("probe-duration.wav", SineWAV(1, 16000))
	if err == nil {
		var secs float64
		if secs, _, err = video.New(sb).Duration(ctx, path); err == nil && math.Abs(secs-1) > 0.1 {
			err = fmt.Errorf("ffprobe read %.3fs from a 1s clip", secs)
		}
	}
	return verdict("ffprobe", err, "read a 1s clip's duration")
}

func modelProblem(key, path string) error {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Errorf("%s %s: %w", key, path, err)
	case st.IsDir():
		return fmt.Errorf("%s %s is a directory, not a model file", key, path)
	}
	return nil
}

func probeVoiceSTT(ctx context.Context, sb sandboxtool.Sandbox, model string) Result {
	const name = "whisper-cli voice.stt.model"
	if strings.TrimSpace(model) == "" {
		return Result{Name: name, Status: StatusSkipped, Detail: "voice.stt.model is not set"}
	}
	if err := modelProblem("voice.stt.model", model); err != nil {
		return verdict(name, err, "")
	}
	stt, err := voice.NewWhisperLocalSTT(voice.WhisperConfig{ModelPath: model, Sandbox: sb})
	if err == nil {
		// A tone has no words: the probe asserts the model loads and a
		// transcript comes back, not what it says.
		_, err = stt.Transcribe(ctx, bytes.NewReader(SineWAV(1, 16000)), voice.Hint{})
	}
	return verdict(name, err, "transcribed a 1s clip with "+filepath.Base(model))
}

func probeAudioExtraction(ctx context.Context, sb sandboxtool.Sandbox, f fixtures, m Models) Result {
	const name = "whisper-cli extractors.audio.model_path"
	switch {
	case strings.TrimSpace(m.AudioExtraction) == "":
		return Result{Name: name, Status: StatusSkipped, Detail: "no audio extraction model (extractors.audio.model_path and voice.stt.model are unset)"}
	case m.AudioExtraction == m.VoiceSTT:
		return Result{Name: name, Status: StatusSkipped, Detail: "the same model as voice.stt.model, probed above"}
	}
	if err := modelProblem("extractors.audio.model_path", m.AudioExtraction); err != nil {
		return verdict(name, err, "")
	}
	path, err := f.write("probe-audio.wav", SineWAV(1, 16000))
	if err == nil {
		_, err = audio.New(sb, m.AudioExtraction).Extract(ctx, extractor.Source{FilePath: path})
		if errors.Is(err, audio.ErrNoSpeech) {
			err = nil // a tone has no speech: the model loaded and ran
		}
	}
	return verdict(name, err, "transcribed a 1s clip with "+filepath.Base(m.AudioExtraction))
}

func probePiper(ctx context.Context, sb sandboxtool.Sandbox, model string) Result {
	const name = "piper"
	if strings.TrimSpace(model) == "" {
		return Result{Name: name, Status: StatusSkipped, Detail: "voice.tts.voice is not set"}
	}
	if err := modelProblem("voice.tts.voice", model); err != nil {
		return verdict(name, err, "")
	}
	if err := modelProblem("voice.tts.voice's .onnx.json", model+".json"); err != nil {
		return verdict(name, err, "")
	}
	tts, err := voice.NewPiperLocalTTS(voice.PiperConfig{ModelPath: model, Sandbox: sb})
	if err == nil {
		var out voice.Audio
		if out, err = tts.Synthesize(ctx, "Vornik doctor check.", voice.TTSOptions{Format: "wav"}); err == nil && out.DurationMs <= 0 {
			err = errors.New("piper produced no audio")
		}
	}
	return verdict(name, err, "synthesised a phrase with "+filepath.Base(model))
}
