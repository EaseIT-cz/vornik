package probe

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// The fixtures are generated, and valid.
func TestFixtures(t *testing.T) {
	pdf := string(PDF("a (b) c"))
	if !strings.HasPrefix(pdf, "%PDF-1.4") || !strings.Contains(pdf, `(a \(b\) c) Tj`) || !strings.HasSuffix(pdf, "%%EOF\n") {
		t.Fatalf("pdf: %q", pdf)
	}
	// The xref offsets point at their objects.
	if i := strings.Index(pdf, "0000000009 00000 n"); i < 0 || !strings.HasPrefix(pdf[9:], "1 0 obj") {
		t.Fatalf("first object offset wrong: %q", pdf)
	}
	img, err := png.Decode(bytes.NewReader(TextPNG(OCRWord, 2)))
	if err != nil || img.Bounds().Dx() != (6*15+12)*2 {
		t.Fatalf("png: %v %v", err, img.Bounds())
	}
	wav := SineWAV(1, 16000)
	if len(wav) != 44+32000 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("wav header: %q", wav[:12])
	}
}

// fakeTools plays every tool the probes run, the way the image does.
func fakeTools(t *testing.T, fail string) *sandboxtest.Fake {
	return sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		if spec.Entrypoint == fail {
			return sandboxtest.Failed(spec.Feature, fail+" broke")
		}
		write := func(name, body string) error { return os.WriteFile(filepath.Join(out, name), []byte(body), 0o600) }
		switch spec.Entrypoint {
		case "pdftotext":
			return write("text.txt", PDFText+"\n\f")
		case "tesseract":
			return write("ocr.txt", OCRWord+"\n")
		case "ffprobe":
			return write("probe.json", `{"format":{"duration":"1.000000"},"streams":[{"codec_type":"audio"}]}`)
		case "ffmpeg":
			last := spec.Args[len(spec.Args)-1]
			return write(filepath.Base(last), string(SineWAV(0.1, 16000)))
		case "whisper-cli":
			return write("transcript.json", `{"result":{"language":"en"},"transcription":[{"offsets":{"from":0,"to":1000},"text":" [BLANK_AUDIO]"}]}`)
		case "piper":
			return write("speech.wav", string(SineWAV(0.5, 22050)))
		}
		return errors.New("unexpected tool " + spec.Entrypoint)
	})
}

func models(t *testing.T) Models {
	dir := t.TempDir()
	for _, f := range []string{"ggml-base.en.bin", "ggml-small.bin", "voice.onnx", "voice.onnx.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("m"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Models{
		VoiceSTT:        filepath.Join(dir, "ggml-base.en.bin"),
		AudioExtraction: filepath.Join(dir, "ggml-small.bin"),
		VoiceTTS:        filepath.Join(dir, "voice.onnx"),
	}
}

func byTool(results []Result) map[string]Result {
	out := map[string]Result{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out
}

// Design §7.1 decision 4: the doctor RUNS each tool and checks what it does,
// through the same code the daemon uses, and whisper-cli once per effective
// model.
func TestRun_ProbesEveryToolFunctionally(t *testing.T) {
	sb := fakeTools(t, "")
	results := Run(context.Background(), sb, models(t))
	got := byTool(results)
	for _, name := range []string{
		"pdftotext", "tesseract", "ffmpeg opus", "ffmpeg aac", "ffprobe",
		"whisper-cli voice.stt.model", "whisper-cli extractors.audio.model_path", "piper",
	} {
		r, ok := got[name]
		if !ok || r.Status != StatusOK {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// Opus and AAC are each encoded (voice_tts) and decoded (voice_stt).
	var encodes, decodes int
	for _, s := range sb.Specs() {
		if s.Entrypoint == "ffmpeg" && s.Feature == sandboxtool.FeatureVoiceTTS {
			encodes++
		}
		if s.Entrypoint == "ffmpeg" && s.Feature == sandboxtool.FeatureVoiceSTT {
			decodes++
		}
	}
	if encodes < 2 || decodes < 2 {
		t.Errorf("opus/aac: %d encodes, %d decodes", encodes, decodes)
	}
}

func TestRun_SkipsUnsetModelsAndASharedModel(t *testing.T) {
	m := models(t)
	m.AudioExtraction = m.VoiceSTT // the fallback: one model, one probe
	m.VoiceTTS = ""
	got := byTool(Run(context.Background(), fakeTools(t, ""), m))
	if got["whisper-cli extractors.audio.model_path"].Status != StatusSkipped {
		t.Errorf("a shared model is probed once: %+v", got["whisper-cli extractors.audio.model_path"])
	}
	if got["piper"].Status != StatusSkipped || !strings.Contains(got["piper"].Detail, "voice.tts.voice") {
		t.Errorf("unset voice: %+v", got["piper"])
	}
	got = byTool(Run(context.Background(), fakeTools(t, ""), Models{}))
	if got["whisper-cli voice.stt.model"].Status != StatusSkipped {
		t.Errorf("unset stt model: %+v", got["whisper-cli voice.stt.model"])
	}
}

func TestRun_AFailingToolAndAnAbsentToolAreToldApart(t *testing.T) {
	got := byTool(Run(context.Background(), fakeTools(t, "tesseract"), models(t)))
	if r := got["tesseract"]; r.Status != StatusFailed || !strings.Contains(r.Detail, "tesseract broke") {
		t.Errorf("failing tesseract: %+v", r)
	}
	if got["pdftotext"].Status != StatusOK {
		t.Error("one failure must not hide the rest")
	}
	absent := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, _ string) error {
		return sandboxtest.NotAvailable(spec.Feature)
	})
	for name, r := range byTool(Run(context.Background(), absent, models(t))) {
		if r.Status != StatusNotAvailable {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// A model path that does not exist fails there, not at the first voice
	// note.
	m := models(t)
	m.VoiceSTT = filepath.Join(t.TempDir(), "missing.bin")
	if r := byTool(Run(context.Background(), fakeTools(t, ""), m))["whisper-cli voice.stt.model"]; r.Status != StatusFailed {
		t.Errorf("missing model: %+v", r)
	}
}

// OCR and text that come back wrong are failures, not passes.
func TestRun_WrongOutputFails(t *testing.T) {
	wrong := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		switch spec.Entrypoint {
		case "pdftotext":
			return os.WriteFile(filepath.Join(out, "text.txt"), []byte("garbage"), 0o600)
		case "tesseract":
			return os.WriteFile(filepath.Join(out, "ocr.txt"), []byte("WORM IE"), 0o600)
		case "ffprobe":
			return os.WriteFile(filepath.Join(out, "probe.json"), []byte(`{"format":{"duration":"7.0"}}`), 0o600)
		}
		return nil
	})
	got := byTool(Run(context.Background(), wrong, Models{}))
	for _, name := range []string{"pdftotext", "tesseract", "ffprobe", "ffmpeg opus", "ffmpeg aac"} {
		if got[name].Status != StatusFailed {
			t.Errorf("%s: %+v", name, got[name])
		}
	}
}
