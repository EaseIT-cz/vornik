//go:build e2e
// +build e2e

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/audio"
	imagex "vornik.io/vornik/internal/extractor/image"
	"vornik.io/vornik/internal/extractor/pdf"
	"vornik.io/vornik/internal/extractor/video"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/probe"
	"vornik.io/vornik/internal/voice"
)

// Process-spawn law S5b (https://docs.vornik.io
// §7): PDF, OCR, video, audio extraction and voice run their tools in the
// real agent image through the sandbox runner. These tests run the real
// tools on real files, assert every run carries --network=none and its
// feature's limits, and assert that a missing tool or model reports "not
// available" while a PATH-first fake of every tool on the daemon side never
// runs.

// Pinned test models, downloaded once into $TMPDIR/vornik-e2e-models and
// verified by sha256; a test that needs one skips, saying so, when offline.
var (
	whisperTinyEn = testModel{
		name:   "ggml-tiny.en.bin",
		url:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-tiny.en.bin",
		sha256: "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f",
	}
	piperLessacLow = testModel{
		name:   "en_US-lessac-low.onnx",
		url:    "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/lessac/low/en_US-lessac-low.onnx",
		sha256: "f7d01dde371555732c4c314111ac79672b1a5ce2fc19266ab42178fd8df7f375",
	}
	piperLessacLowJSON = testModel{
		name:   "en_US-lessac-low.onnx.json",
		url:    "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/lessac/low/en_US-lessac-low.onnx.json",
		sha256: "45754dfdebb3b8661c3fc564713772deec6e064feeb5b4e9594857dc7305193a",
	}
)

type testModel struct{ name, url, sha256 string }

// fetchModel returns the model's path in the cache, downloading it when
// absent. Offline (or any download failure) skips the test; a sha256
// mismatch fails it.
func fetchModel(t *testing.T, m testModel) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "vornik-e2e-models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, m.name)
	if sum, err := fileSHA256(path); err == nil && sum == m.sha256 {
		return path
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(m.url)
	if err != nil {
		t.Skipf("model %s not cached and cannot be downloaded (offline?): %v", m.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("model %s: download returned %s", m.name, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, m.name+".part-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		t.Skipf("model %s: download interrupted: %v", m.name, err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if sum, _ := fileSHA256(tmp.Name()); sum != m.sha256 {
		t.Fatalf("model %s: sha256 %s, want the pinned %s", m.name, sum, m.sha256)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hostToolTrap puts a fake of every sandbox tool first on PATH, each leaving
// a marker if anything runs it on the host.
func hostToolTrap(t *testing.T) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "host-tool-ran")
	script := "#!/bin/sh\necho \"$0\" >> '" + marker + "'\nexit 1\n"
	for _, tool := range []string{"pdftotext", "tesseract", "ffmpeg", "ffprobe", "whisper", "whisper-cli", "whisper-cpp", "main", "piper"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func noHostToolRan(t *testing.T, marker string) {
	t.Helper()
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a host tool ran: %s", b)
	}
}

func mediaRunner(t *testing.T, image string, rec *recordingRunner) *sandboxtool.Runner {
	t.Helper()
	r, err := sandboxtool.New(sandboxtool.Config{Image: image, ScratchRoot: t.TempDir()}, rec.run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// assertBounded checks every container the runner started carries the
// fixed hardening and its feature's limits.
func assertBounded(t *testing.T, r *sandboxtool.Runner, rec *recordingRunner, image string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) == 0 {
		t.Fatal("no sandbox run was recorded")
	}
	for _, call := range rec.calls {
		joined := strings.Join(call, " ")
		feature := featureOf(t, r, call)
		mem := strconv.FormatInt(r.Memory(feature), 10)
		for _, want := range []string{
			"--network=none", "--pull=never", "--userns=keep-id", "--cap-drop=ALL",
			"--security-opt=no-new-privileges", "--read-only", "--tmpfs /tmp:rw,size=256m",
			"--memory=" + mem, "--memory-swap=" + mem, "--pids-limit=64", "--cpus=" + r.CPUs(feature),
			"--label " + sandboxtool.RunLabel + "=", image,
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv lacks %q: %s", want, joined)
			}
		}
	}
}

// featureOf recovers a run's feature from its entrypoint and limits: the
// argv carries no feature name, so match the entrypoint to the features that
// use it and the memory and CPU flags to theirs.
func featureOf(t *testing.T, r *sandboxtool.Runner, call []string) sandboxtool.Feature {
	t.Helper()
	var entry, mem, cpus string
	for i, a := range call {
		if a == "--entrypoint" && i+1 < len(call) {
			entry = call[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--memory="); ok {
			mem = v
		}
		if v, ok := strings.CutPrefix(a, "--cpus="); ok {
			cpus = v
		}
	}
	candidates := map[string][]sandboxtool.Feature{
		"pdftotext":   {sandboxtool.FeaturePDF},
		"tesseract":   {sandboxtool.FeatureImageOCR},
		"ffprobe":     {sandboxtool.FeatureVideo},
		"ffmpeg":      {sandboxtool.FeatureVideo, sandboxtool.FeatureAudio, sandboxtool.FeatureVoiceSTT, sandboxtool.FeatureVoiceTTS},
		"whisper-cli": {sandboxtool.FeatureAudio, sandboxtool.FeatureVoiceSTT},
		"piper":       {sandboxtool.FeatureVoiceTTS},
	}[entry]
	for _, f := range candidates {
		if strconv.FormatInt(r.Memory(f), 10) == mem && r.CPUs(f) == cpus {
			return f
		}
	}
	t.Fatalf("run of %q with --memory=%s --cpus=%s matches no feature: %v", entry, mem, cpus, call)
	return ""
}

func TestSandboxMediaE2E_ExtractsRealFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	marker := hostToolTrap(t)
	rec := &recordingRunner{}
	r := mediaRunner(t, image, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()

	// A real PDF: pdftotext in the sandbox returns its text.
	pdfPath := filepath.Join(dir, "the report (final).pdf")
	if err := os.WriteFile(pdfPath, probe.PDF(probe.PDFText), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := pdf.New(r).Extract(ctx, extractor.Source{FilePath: pdfPath, OriginalName: "the report (final).pdf"})
	if err != nil || len(res.Sections) != 1 || !strings.Contains(res.Sections[0].Content, probe.PDFText) {
		t.Fatalf("pdf: %v %+v", err, res.Sections)
	}

	// A PNG with rendered text: tesseract in the sandbox reads it.
	pngPath := filepath.Join(dir, "whiteboard.png")
	if err := os.WriteFile(pngPath, probe.TextPNG(probe.OCRWord, 5), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = imagex.New(r).Extract(ctx, extractor.Source{FilePath: pngPath})
	if err != nil || res.Metadata.Extra["ocr_engine"] != "tesseract" || !strings.Contains(res.Sections[0].Content, probe.OCRWord) {
		t.Fatalf("ocr: %v %+v", err, res)
	}

	// A short video with an audio track, made by the image's own ffmpeg:
	// ffprobe reads its streams and ffmpeg samples its frames.
	clip := makeClip(t, ctx, r, dir)
	res, err = video.New(r).Extract(ctx, extractor.Source{FilePath: clip, OriginalName: "holiday clip.mp4"})
	if err != nil {
		t.Fatalf("video: %v", err)
	}
	x := res.Metadata.Extra
	if x["width"] != "160" || x["height"] != "120" || x["has_audio_track"] != "true" || res.Metadata.DurationSeconds != 2 {
		t.Fatalf("video metadata: %+v", res.Metadata)
	}
	if len(res.Files) == 0 || !bytes.HasPrefix(res.Files[0].Content, []byte{0xff, 0xd8}) {
		t.Fatalf("video frames: %d files, first %q", len(res.Files), firstBytes(firstFile(res)))
	}

	assertBounded(t, r, rec, image)
	for _, call := range rec.calls {
		joined := strings.Join(call, " ")
		for _, userText := range []string{"report", "final", "whiteboard", "holiday"} {
			if strings.Contains(joined, userText) {
				t.Errorf("a file name reached argv: %s", joined)
			}
		}
	}
	noHostToolRan(t, marker)
}

func firstFile(res extractor.Result) []byte {
	if len(res.Files) == 0 {
		return nil
	}
	return res.Files[0].Content
}

// makeClip renders a 2 s 160x120 test pattern with a tone, in the sandbox.
func makeClip(t *testing.T, ctx context.Context, r *sandboxtool.Runner, dir string) string {
	t.Helper()
	out, err := r.Run(ctx, sandboxtool.Spec{
		Feature: sandboxtool.FeatureVideo, Entrypoint: "ffmpeg",
		// The thread caps every production ffmpeg run carries: without
		// them this run exceeded --pids-limit=64 on a 16-core host and hung
		// until its timeout (sandboxtool.FFmpegThreads).
		Args: []string{"-nostdin", "-loglevel", "error",
			"-threads", sandboxtool.FFmpegThreads, "-filter_threads", sandboxtool.FFmpegThreads,
			"-f", "lavfi", "-i", "testsrc=duration=2:size=160x120:rate=10",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
			"-threads", sandboxtool.FFmpegThreads,
			"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "/out/generated.mp4"},
	})
	if err != nil {
		t.Fatalf("make clip: %v", err)
	}
	defer out.Close()
	data, err := os.ReadFile(filepath.Join(out.OutDir, "generated.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "holiday clip.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Voice both ways and audio extraction, with real models: piper speaks a
// phrase, it is sent back through the Telegram (Opus) and Slack (AAC)
// paths, and whisper-cli transcribes it; the audio extractor transcribes the
// same speech; and the doctor's functional probes pass with each model.
func TestSandboxMediaE2E_VoiceAndAudioWithRealModels(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	whisperModel := fetchModel(t, whisperTinyEn)
	piperModel := fetchModel(t, piperLessacLow)
	fetchModel(t, piperLessacLowJSON)
	marker := hostToolTrap(t)
	rec := &recordingRunner{}
	r := mediaRunner(t, image, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tts, err := voice.NewPiperLocalTTS(voice.PiperConfig{ModelPath: piperModel, Sandbox: r})
	if err != nil {
		t.Fatal(err)
	}
	stt, err := voice.NewWhisperLocalSTT(voice.WhisperConfig{ModelPath: whisperModel, Sandbox: r})
	if err != nil {
		t.Fatal(err)
	}
	const phrase = "Hello world, this is a test."
	for _, format := range []string{"ogg-opus", "mp4-aac"} {
		reply, err := tts.Synthesize(ctx, phrase, voice.TTSOptions{Format: format})
		if err != nil || len(reply.Bytes) == 0 || reply.DurationMs <= 0 {
			t.Fatalf("synthesize %s: %v (%d bytes)", format, err, len(reply.Bytes))
		}
		// The reply, fed back as an inbound voice note, transcribes.
		tr, err := stt.Transcribe(ctx, bytes.NewReader(reply.Bytes), voice.Hint{})
		if err != nil {
			t.Fatalf("transcribe %s: %v", format, err)
		}
		if !heardPhrase(tr.Text) {
			t.Errorf("transcript of the %s reply = %q, want the phrase (2 of hello/world/test)", format, tr.Text)
		}
	}

	wav, err := tts.Synthesize(ctx, phrase, voice.TTSOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	speech := filepath.Join(t.TempDir(), "memo.wav")
	if err := os.WriteFile(speech, wav.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := audio.New(r, whisperModel).Extract(ctx, extractor.Source{FilePath: speech, OriginalName: "memo.wav"})
	if err != nil || len(res.Sections) == 0 || !heardPhrase(res.Sections[0].Content) {
		t.Fatalf("audio extraction: %v %+v", err, res.Sections)
	}

	// vornikctl doctor's functional probes, against the real image.
	for _, p := range probe.Run(ctx, r, probe.Models{VoiceSTT: whisperModel, AudioExtraction: whisperModel, VoiceTTS: piperModel}) {
		if p.Status != probe.StatusOK && p.Status != probe.StatusSkipped {
			t.Errorf("doctor probe %s: %s %s", p.Name, p.Status, p.Detail)
		}
	}

	assertBounded(t, r, rec, image)
	for _, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "Hello world") {
			t.Errorf("the spoken text reached argv: %v", call)
		}
	}
	noHostToolRan(t, marker)
}

// Incident 2026-10-03 (voice-messages-design.md §8), against the real image:
// a file with no audio stream (a PNG) fails the normalise run with
// ErrNoAudioStream, bytes no demuxer recognises fail it with ffmpeg's own
// message, neither reaches whisper-cli, and a real Opus note
// normalises to the canonical, bitexact 44-byte-header WAV that CheckWAV
// accepts. No model is downloaded: the failing inputs stop before
// whisper-cli, and the Opus note runs NormaliseSpec on its own.
func TestSandboxMediaE2E_UndecodableInputSaysWhy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	marker := hostToolTrap(t)
	rec := &recordingRunner{}
	r := mediaRunner(t, image, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	model := filepath.Join(t.TempDir(), "ggml-unused.bin")
	if err := os.WriteFile(model, []byte("never loaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	stt, err := voice.NewWhisperLocalSTT(voice.WhisperConfig{ModelPath: model, Sandbox: r})
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 4000)
	for i := range random {
		random[i] = byte((i*7919 + 13) % 251)
	}
	// A PNG is a file ffmpeg reads, with a video stream and no audio one.
	if _, err := stt.Transcribe(ctx, bytes.NewReader(probe.TextPNG("NO AUDIO", 2)), voice.Hint{}); !errors.Is(err, voice.ErrNoAudioStream) {
		t.Errorf("png: err = %v, want ErrNoAudioStream", err)
	}
	// Bytes no demuxer recognises are a decode failure that carries
	// ffmpeg's words, not "no audio stream".
	_, err = stt.Transcribe(ctx, bytes.NewReader(random), voice.Hint{})
	if err == nil || errors.Is(err, voice.ErrNoAudioStream) || !strings.Contains(err.Error(), "Invalid data") {
		t.Errorf("random bytes: err = %v, want ffmpeg's decode failure", err)
	}
	rec.mu.Lock()
	for _, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "whisper-cli") {
			t.Errorf("whisper-cli ran on input with no audio: %v", call)
		}
	}
	rec.mu.Unlock()

	wavPath := filepath.Join(t.TempDir(), "tone.wav")
	if err := os.WriteFile(wavPath, probe.SineWAV(1, 16000), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, output, err := voice.TranscodeSpec(wavPath, "ogg-opus")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := r.Run(ctx, spec)
	if err != nil {
		t.Fatalf("encode opus: %v", err)
	}
	opus, err := os.ReadFile(filepath.Join(enc.OutDir, output))
	enc.Close()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := r.Run(ctx, voice.NormaliseSpec(opus))
	if err != nil {
		t.Fatalf("normalise opus: %v", err)
	}
	defer dec.Close()
	out := filepath.Join(dec.OutDir, "audio.wav")
	if err := voice.CheckWAV(out); err != nil {
		t.Fatalf("normalised opus: %v", err)
	}
	wav, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Errorf("want the canonical 44-byte header (no LIST chunk), got %q", wav[:48])
	}
	assertBounded(t, r, rec, image)
	noHostToolRan(t, marker)
}

// §7.1 decision 4: a tool the image does not declare, an image that is not
// there, and a model that is not configured each report "not available" —
// and nothing runs on the host.
func TestSandboxMediaE2E_NotAvailableNeverFallsBackToTheHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	marker := hostToolTrap(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "w.png")
	pdfPath := filepath.Join(dir, "d.pdf")
	wavPath := filepath.Join(dir, "a.wav")
	for path, data := range map[string][]byte{pngPath: probe.TextPNG(probe.OCRWord, 5), pdfPath: probe.PDF(probe.PDFText), wavPath: probe.SineWAV(1, 16000)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// An image whose label declares only pdftotext: OCR is not available
	// and starts no container; PDF still works.
	trimmed := trimmedLabelImage(t, image)
	rec := &recordingRunner{}
	r := mediaRunner(t, trimmed, rec)
	res, err := imagex.New(r).Extract(ctx, extractor.Source{FilePath: pngPath})
	if err != nil || !strings.Contains(res.Sections[0].Content, "OCR not available in the agent image") {
		t.Fatalf("undeclared tesseract: %v %+v", err, res.Sections)
	}
	if _, err := video.New(r).Extract(ctx, extractor.Source{FilePath: wavPath}); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("undeclared ffprobe: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("an undeclared tool must start no container: %v", rec.calls)
	}
	if _, err := pdf.New(r).Extract(ctx, extractor.Source{FilePath: pdfPath}); err != nil {
		t.Fatalf("a declared tool still runs: %v", err)
	}

	// An image that is not there.
	gone := mediaRunner(t, "localhost/vornik-agent:does-not-exist-e2e", &recordingRunner{})
	if _, err := pdf.New(gone).Extract(ctx, extractor.Source{FilePath: pdfPath}); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("missing image: %v", err)
	}
	stt, _ := voice.NewWhisperLocalSTT(voice.WhisperConfig{ModelPath: "/nonexistent/ggml.bin", Sandbox: gone})
	if _, err := stt.Transcribe(ctx, bytes.NewReader(probe.SineWAV(1, 16000)), voice.Hint{}); !errors.Is(err, voice.ErrProviderUnavailable) {
		t.Fatalf("voice with a missing image: %v", err)
	}
	tts, _ := voice.NewPiperLocalTTS(voice.PiperConfig{ModelPath: "/nonexistent/v.onnx", Sandbox: gone})
	if _, err := tts.Synthesize(ctx, "hi", voice.TTSOptions{}); !errors.Is(err, voice.ErrProviderUnavailable) {
		t.Fatalf("tts with a missing image: %v", err)
	}

	// No model configured: audio extraction is not available.
	if _, err := audio.New(mediaRunner(t, image, &recordingRunner{}), "").Extract(ctx, extractor.Source{FilePath: wavPath}); !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no model: %v", err)
	}
	noHostToolRan(t, marker)
}

// S5a review F1: a daemon killed mid-run leaves its named container; the
// next start's sweep removes it — and only its own daemon's, never another
// runner's run in flight.
func TestSandboxMediaE2E_SweepRemovesOnlyThisRunnersLeftovers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox e2e requires Linux + podman")
	}
	image := buildAgentImage(t)
	r := mediaRunner(t, image, &recordingRunner{})
	other := mediaRunner(t, image, &recordingRunner{})
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mine, theirs := "vornik-sbx-e2e-mine-"+suffix, "vornik-sbx-e2e-theirs-"+suffix
	for name, scope := range map[string]string{mine: r.Scope(), theirs: other.Scope()} {
		out, err := exec.Command("podman", "create", "--name", name,
			"--label", sandboxtool.RunLabel+"="+scope, "--entrypoint", "true", image).CombinedOutput()
		if err != nil {
			t.Fatalf("create %s: %v\n%s", name, err, out)
		}
	}
	t.Cleanup(func() { // bounded, as in trimmedLabelImage
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, "podman", "rm", "-f", "--ignore", mine, theirs).Run()
	})

	n, err := r.SweepContainers(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("swept %d (%v), want 1", n, err)
	}
	if exec.Command("podman", "container", "exists", mine).Run() == nil {
		t.Error("this runner's leftover must be removed")
	}
	if exec.Command("podman", "container", "exists", theirs).Run() != nil {
		t.Error("another runner's container must be left alone")
	}
}

// trimmedLabelImage derives an image from the agent image whose sandbox-tools
// label declares only pdftotext: a metadata-only layer, built without a pull.
func trimmedLabelImage(t *testing.T, base string) string {
	t.Helper()
	tag := "localhost/vornik-agent:e2e-label-trimmed"
	dir := t.TempDir()
	containerfile := fmt.Sprintf("FROM %s\nLABEL %s=pdftotext\n", base, sandboxtool.ToolsLabel)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("podman", "build", "--pull=never", "-t", tag, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("build trimmed-label image: %v\n%s", err, out)
	}
	// Bounded: on the 2026-10-01 CI run (36910205508) a timed-out tool run
	// left podman wedged, and this unbounded rmi hung until the package's
	// test timeout, hiding the real assertion behind a panic.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, "podman", "rmi", "-f", tag).Run()
	})
	return tag
}

// heardPhrase reports whether a transcript of "Hello world, this is a test."
// carries the phrase: at least two of its three content words. whisper-tiny
// on synthesized speech sometimes hears the first word as "Below" (the 2026.9.7
// release gate, 1 run in 4), so pinning one word made the gate a coin flip,
// while an empty or unrelated transcript still fails.
func heardPhrase(transcript string) bool {
	words := strings.FieldsFunc(strings.ToLower(transcript), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	seen := map[string]bool{}
	for _, w := range words {
		seen[w] = true
	}
	n := 0
	for _, w := range []string{"hello", "world", "test"} {
		if seen[w] {
			n++
		}
	}
	return n >= 2
}

// The transcript that failed the 2026.9.7 release gate must count as heard;
// an empty or unrelated one must not.
func TestHeardPhrase(t *testing.T) {
	for in, want := range map[string]bool{
		"Below world, this is a test.": true,
		"Hello world, this is a test.": true,
		"hello, WORLD":                 true,
		"":                             false,
		"Below":                        false,
		"[BLANK_AUDIO]":                false,
		"the weather is fine today":    false,
	} {
		if got := heardPhrase(in); got != want {
			t.Errorf("heardPhrase(%q) = %v, want %v", in, got, want)
		}
	}
}
