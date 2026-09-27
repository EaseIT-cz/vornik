package cli

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// Process-spawn law S5b: `vornikctl doctor` probes whisper-cli with EACH
// effective model (design §7.1 decision 4, S5-R3C) and piper with the
// configured voice, under the daemon's sandbox limits.
func TestSandboxEnvFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.SandboxTools.MaxConcurrent = 3
	cfg.Voice.STT.Provider = "whisper-local"
	cfg.Voice.STT.Model = "/m/ggml-base.en.bin"
	cfg.Voice.TTS.Provider = "piper"
	cfg.Voice.TTS.Voice = "/v/en_US-lessac-low.onnx"
	cfg.Extractors.Audio.ModelPath = "/m/ggml-small.bin"
	env := sandboxEnvFromConfig(cfg)
	if env.Tools.MaxConcurrent != 3 || env.Models.VoiceSTT != "/m/ggml-base.en.bin" ||
		env.Models.AudioExtraction != "/m/ggml-small.bin" || env.Models.VoiceTTS != "/v/en_US-lessac-low.onnx" {
		t.Fatalf("env = %+v", env)
	}

	// Voice off, its model still set: the audio extractor falls back to it,
	// so that model is probed once, as the audio extraction model.
	cfg = &config.Config{}
	cfg.Voice.STT.Model = "/m/ggml-base.en.bin"
	cfg.Voice.TTS.Voice = "/v/x.onnx"
	env = sandboxEnvFromConfig(cfg)
	if env.Models.VoiceSTT != "" || env.Models.VoiceTTS != "" || env.Models.AudioExtraction != "/m/ggml-base.en.bin" {
		t.Fatalf("env = %+v", env)
	}
}
