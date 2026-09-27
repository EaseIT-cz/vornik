package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// piper and its ffmpeg transcode run in the agent image through the sandbox
// runner (process-spawn law S5b, design §7): a fake sandbox plays them here,
// and the real tools run under the podman e2e lane.

// makeWAV builds a minimal 16-bit PCM RIFF/WAVE blob with the given
// sample rate and duration. Kept as a helper rather than a testdata
// fixture so the test stays hermetic.
func makeWAV(sampleRate, durationSamples int) []byte {
	bytesPerSample := 2 // 16-bit PCM, mono
	dataBytes := durationSamples * bytesPerSample
	out := make([]byte, 0, 44+dataBytes)
	out = append(out, []byte("RIFF")...)
	out = appendU32LE(out, uint32(dataBytes+36))
	out = append(out, []byte("WAVE")...)
	out = append(out, []byte("fmt ")...)
	out = appendU32LE(out, 16)                   // PCM chunk size
	out = appendU16LE(out, 1)                    // audioFormat=1 (PCM)
	out = appendU16LE(out, 1)                    // channels
	out = appendU32LE(out, uint32(sampleRate))   // sample rate
	out = appendU32LE(out, uint32(sampleRate*2)) // byte rate
	out = appendU16LE(out, 2)                    // block align
	out = appendU16LE(out, 16)                   // bits per sample
	out = append(out, []byte("data")...)
	out = appendU32LE(out, uint32(dataBytes))
	out = append(out, make([]byte, dataBytes)...)
	return out
}

func appendU32LE(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

func appendU16LE(b []byte, v uint16) []byte {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	return append(b, buf[:]...)
}

// piperTools plays piper (writing wav to /out/speech.wav) and ffmpeg
// (writing the transcoded reply). Either may be replaced by an error.
type piperTools struct {
	wav       []byte
	piperErr  error
	ffmpegOut []byte
	ffmpegErr error
}

func (p piperTools) fake(t *testing.T) *sandboxtest.Fake {
	return sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		switch spec.Entrypoint {
		case "piper":
			if p.piperErr != nil {
				return p.piperErr
			}
			if p.wav == nil {
				return nil
			}
			return os.WriteFile(filepath.Join(out, "speech.wav"), p.wav, 0o600)
		case "ffmpeg":
			if p.ffmpegErr != nil {
				return p.ffmpegErr
			}
			if p.ffmpegOut == nil {
				return nil
			}
			name := filepath.Base(spec.Args[len(spec.Args)-1])
			return os.WriteFile(filepath.Join(out, name), p.ffmpegOut, 0o600)
		}
		return errors.New("unexpected tool " + spec.Entrypoint)
	})
}

func newPiper(t *testing.T, sb sandboxtool.Sandbox, cfg PiperConfig) *piperLocalTTS {
	t.Helper()
	if cfg.ModelPath == "" {
		cfg.ModelPath = "en_US-lessac-low.onnx"
	}
	cfg.ModelPath = modelFile(t, cfg.ModelPath)
	cfg.Sandbox = sb
	prov, err := NewPiperLocalTTS(cfg)
	if err != nil {
		t.Fatalf("NewPiperLocalTTS: %v", err)
	}
	return prov.(*piperLocalTTS)
}

func TestPiperLocalTTS_New_RequiresModelPath(t *testing.T) {
	if _, err := NewPiperLocalTTS(PiperConfig{}); err == nil {
		t.Fatal("expected error when ModelPath is empty; got nil")
	}
}

func TestPiperLocalTTS_New_AppliesDefaults(t *testing.T) {
	prov, err := NewPiperLocalTTS(PiperConfig{ModelPath: "/tmp/voice.onnx"})
	if err != nil {
		t.Fatalf("NewPiperLocalTTS: %v", err)
	}
	p := prov.(*piperLocalTTS)
	if p.cfg.DefaultVoice != defaultPiperVoice {
		t.Errorf("DefaultVoice = %q, want %q", p.cfg.DefaultVoice, defaultPiperVoice)
	}
	if p.cfg.DefaultSpeed != defaultPiperSpeed {
		t.Errorf("DefaultSpeed = %v, want %v", p.cfg.DefaultSpeed, defaultPiperSpeed)
	}
	if p.cfg.MaxTextRunes != defaultPiperMaxRunes {
		t.Errorf("MaxTextRunes = %d, want %d", p.cfg.MaxTextRunes, defaultPiperMaxRunes)
	}
}

func TestPiperLocalTTS_Synthesize_EmptyText(t *testing.T) {
	p := newPiper(t, piperTools{}.fake(t), PiperConfig{})
	for _, in := range []string{"", "   ", "\t\n"} {
		_, err := p.Synthesize(context.Background(), in, TTSOptions{})
		if !errors.Is(err, ErrEmptyText) {
			t.Errorf("Synthesize(%q) err = %v, want ErrEmptyText", in, err)
		}
	}
}

func TestPiperLocalTTS_Synthesize_OversizeText(t *testing.T) {
	p := newPiper(t, piperTools{}.fake(t), PiperConfig{MaxTextRunes: 10})
	_, err := p.Synthesize(context.Background(), strings.Repeat("a", 100), TTSOptions{})
	if !errors.Is(err, ErrOversizeText) {
		t.Errorf("err = %v, want ErrOversizeText", err)
	}
}

// §7.1 decision 4: no sandbox, or an image without piper, reports the
// provider unavailable and never runs a host piper.
func TestPiperLocalTTS_Synthesize_NotAvailableNeverRunsTheHost(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, tool := range []string{"piper", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := newPiper(t, nil, PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hello", TTSOptions{}); !errors.Is(err, ErrProviderUnavailable) || !errors.Is(err, sandboxtool.ErrNotAvailable) {
		t.Fatalf("no sandbox: err = %v", err)
	}
	p = newPiper(t, piperTools{piperErr: sandboxtest.NotAvailable(sandboxtool.FeatureVoiceTTS)}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hello", TTSOptions{}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("undeclared piper: err = %v, want ErrProviderUnavailable", err)
	}
	p = newPiper(t, piperTools{wav: makeWAV(22050, 100), ffmpegErr: sandboxtest.NotAvailable(sandboxtool.FeatureVoiceTTS)}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hello", TTSOptions{Format: "ogg-opus"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("undeclared ffmpeg: err = %v, want ErrProviderUnavailable", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a host piper/ffmpeg ran")
	}
}

func TestPiperLocalTTS_Synthesize_ToolFailureSurfaces(t *testing.T) {
	p := newPiper(t, piperTools{piperErr: sandboxtest.Failed(sandboxtool.FeatureVoiceTTS, "Unable to load voice")}.fake(t), PiperConfig{})
	_, err := p.Synthesize(context.Background(), "hello", TTSOptions{})
	if err == nil || !strings.Contains(err.Error(), "Unable to load voice") {
		t.Errorf("err = %v, want piper's message", err)
	}
}

func TestPiperLocalTTS_Synthesize_EmptyOutput(t *testing.T) {
	p := newPiper(t, piperTools{}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hello", TTSOptions{}); err == nil {
		t.Error("expected error on missing output")
	}
}

func TestPiperLocalTTS_Synthesize_NotAWAV(t *testing.T) {
	p := newPiper(t, piperTools{wav: []byte("not a wav at all, definitely not RIFF, padded to be long enough")}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hello", TTSOptions{}); err == nil {
		t.Error("expected WAV parse error")
	}
}

// The run is the fixed voice_tts shape: the text rides stdin from a file,
// never argv; the voice's DIRECTORY is mounted at /models.
func TestPiperLocalTTS_Synthesize_WAVOutput(t *testing.T) {
	wav := makeWAV(22050, 22050) // 1 second
	sb := piperTools{wav: wav}.fake(t)
	p := newPiper(t, sb, PiperConfig{ModelPath: "/var/lib/vornik/voice/en_US-lessac-low.onnx"})
	out, err := p.Synthesize(context.Background(), "hello --model /etc/passwd", TTSOptions{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if out.MimeType != "audio/wav" || out.SampleRateHz != 22050 || out.DurationMs != 1000 || len(out.Bytes) != len(wav) {
		t.Errorf("audio = %+v", out)
	}
	specs := sb.Specs()
	if len(specs) != 1 {
		t.Fatalf("wav needs no transcode run, got %d runs", len(specs))
	}
	spec := specs[0]
	if spec.Feature != sandboxtool.FeatureVoiceTTS || spec.Entrypoint != "piper" ||
		spec.ModelDir != filepath.Dir(p.cfg.ModelPath) || spec.Stdin != "text" {
		t.Fatalf("run = %+v", spec)
	}
	if got := strings.Join(spec.Args, " "); got != "--model /models/en_US-lessac-low.onnx --length_scale 1.0000 --output_file /out/speech.wav --quiet" {
		t.Fatalf("argv = %q", got)
	}
	if len(spec.Inputs) != 1 || spec.Inputs[0].Name != "text" || string(spec.Inputs[0].Data) != "hello --model /etc/passwd" {
		t.Fatalf("the text must ride stdin: %+v", spec.Inputs)
	}
}

func TestPiperLocalTTS_Synthesize_SpeedPlumbing(t *testing.T) {
	sb := piperTools{wav: makeWAV(22050, 100)}.fake(t)
	p := newPiper(t, sb, PiperConfig{DefaultSpeed: 1.0})
	if _, err := p.Synthesize(context.Background(), "hi", TTSOptions{Speed: 2.0}); err != nil {
		t.Fatal(err)
	}
	if !sliceContains(sb.Specs()[0].Args, "--length_scale", "0.5000") {
		t.Errorf("speed 2.0 must become length_scale 0.5: %v", sb.Specs()[0].Args)
	}
}

// Telegram's Opus and Slack's AAC replies keep working: the transcode is a
// second voice_tts run on piper's WAV.
func TestPiperLocalTTS_Synthesize_Transcodes(t *testing.T) {
	for _, tc := range []struct {
		format, mime, output string
		wantArgs             []string
	}{
		{"ogg-opus", "audio/ogg", "/out/reply.ogg", []string{"-c:a", "libopus", "-f", "ogg"}},
		{"mp4-aac", "audio/mp4", "/out/reply.m4a", []string{"-c:a", "aac", "-f", "mp4"}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			sb := piperTools{wav: makeWAV(22050, 22050), ffmpegOut: []byte("ENCODED")}.fake(t)
			p := newPiper(t, sb, PiperConfig{})
			out, err := p.Synthesize(context.Background(), "hello", TTSOptions{Format: tc.format})
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if out.MimeType != tc.mime || string(out.Bytes) != "ENCODED" || out.DurationMs != 1000 {
				t.Errorf("audio = %+v", out)
			}
			specs := sb.Specs()
			if len(specs) != 2 {
				t.Fatalf("want piper then ffmpeg, got %d runs", len(specs))
			}
			ff := specs[1]
			if ff.Feature != sandboxtool.FeatureVoiceTTS || ff.Entrypoint != "ffmpeg" || ff.Args[len(ff.Args)-1] != tc.output {
				t.Fatalf("transcode run = %+v", ff)
			}
			// The pids bound needs ffmpeg's threads capped (e2e, 2026-09-26).
			if !sliceContains(ff.Args, "-threads", "2", "-filter_threads", "2") ||
				!sliceContains(ff.Args, "-i", "/in/speech.wav", "-threads", "2") || !sliceContains(ff.Args, tc.wantArgs[0], tc.wantArgs[1]) ||
				!sliceContains(ff.Args, tc.wantArgs[2], tc.wantArgs[3]) {
				t.Fatalf("transcode argv = %v", ff.Args)
			}
			if len(ff.Inputs) != 1 || ff.Inputs[0].Name != "speech.wav" {
				t.Fatalf("transcode input = %+v", ff.Inputs)
			}
		})
	}
}

func TestPiperLocalTTS_Synthesize_UnknownFormat(t *testing.T) {
	p := newPiper(t, piperTools{wav: makeWAV(22050, 100)}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "flac"}); err == nil {
		t.Error("expected error on unknown format")
	}
	if _, err := p.transcode(context.Background(), "/x.wav", "flac"); err == nil {
		t.Error("transcode must refuse an unknown format")
	}
}

func TestPiperLocalTTS_Synthesize_FfmpegFails(t *testing.T) {
	p := newPiper(t, piperTools{wav: makeWAV(22050, 100), ffmpegErr: sandboxtest.Failed(sandboxtool.FeatureVoiceTTS, "Unknown encoder 'libopus'")}.fake(t), PiperConfig{})
	_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "ogg-opus"})
	if err == nil || !strings.Contains(err.Error(), "Unknown encoder") {
		t.Errorf("err = %v, want ffmpeg's message", err)
	}
}

func TestPiperLocalTTS_Synthesize_FfmpegEmptyOutput(t *testing.T) {
	p := newPiper(t, piperTools{wav: makeWAV(22050, 100), ffmpegOut: []byte{}}.fake(t), PiperConfig{})
	if _, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "mp4-aac"}); err == nil {
		t.Error("expected error on empty transcode output")
	}
}

func TestParseWAV_TooShort(t *testing.T) {
	if _, _, err := parseWAV([]byte("RIFF")); err == nil {
		t.Error("expected error on short input")
	}
}

func TestParseWAV_BadMagic(t *testing.T) {
	b := makeWAV(22050, 10)
	copy(b[0:4], "XXXX")
	if _, _, err := parseWAV(b); err == nil {
		t.Error("expected error on bad RIFF magic")
	}
}

func TestParseWAV_OK(t *testing.T) {
	sr, dur, err := parseWAV(makeWAV(16000, 8000))
	if err != nil {
		t.Fatalf("parseWAV: %v", err)
	}
	if sr != 16000 || dur != 500 {
		t.Errorf("sr=%d dur=%d, want 16000/500", sr, dur)
	}
}

func TestRunesIn(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hi", 2},
		{"héllo", 5},
		{"日本語", 3},
	}
	for _, tc := range cases {
		if got := runesIn(tc.in); got != tc.want {
			t.Errorf("runesIn(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// sliceContains reports whether `args` contains the pair in sequence.
func sliceContains(args []string, pair ...string) bool {
	for i := 0; i+len(pair) <= len(args); i++ {
		ok := true
		for j, p := range pair {
			if args[i+j] != p {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// modelFile writes an empty model file named after path's base into a temp
// directory: the providers pre-flight their model on every call (process-spawn
// law S5b review residual R4), so a test's model has to exist.
func modelFile(t *testing.T, path string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(p, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// S5b review residual R4 (review-20260926-c245 F2): piper did not pre-flight
// its model per call, unlike the audio extractor, so a voice model removed
// after startup failed as an opaque podman mount error. Now it is
// ErrProviderUnavailable naming the file, and no container starts.
func TestVoiceProviders_AMissingModelIsUnavailableAndStartsNothing(t *testing.T) {
	sb := piperTools{wav: makeWAV(22050, 100)}.fake(t)
	p := newPiper(t, sb, PiperConfig{})
	gone := p.cfg.ModelPath
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	_, err := p.Synthesize(context.Background(), "hello", TTSOptions{})
	if !errors.Is(err, ErrProviderUnavailable) || !strings.Contains(err.Error(), gone) {
		t.Fatalf("piper with its model gone: want ErrProviderUnavailable naming %s, got %v", gone, err)
	}
	if n := len(sb.Specs()); n != 0 {
		t.Fatalf("a missing model must start no container, got %d runs", n)
	}

	wsb := whisperTools{wav: makeWAV(16000, 100), transcript: helloJSON}.fake(t)
	w := newWhisper(t, wsb, WhisperConfig{})
	dir := w.(*whisperLocalSTT).cfg.ModelPath
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil { // a directory where the file was
		t.Fatal(err)
	}
	_, err = w.Transcribe(context.Background(), strings.NewReader("OggS"), Hint{})
	if !errors.Is(err, ErrProviderUnavailable) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("whisper with a directory for a model: want ErrProviderUnavailable naming %s, got %v", dir, err)
	}
	if n := len(wsb.Specs()); n != 0 {
		t.Fatalf("a missing model must start no container, got %d runs", n)
	}
}
