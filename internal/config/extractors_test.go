package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// Design §7.1 decision 3 (process-spawn law S5b): audio extraction runs
// whisper.cpp with extractors.audio.model_path, falling back to the voice
// model, so an operator who configured voice gets audio extraction with the
// same model; with neither set it is not available.
func TestExtractorsConfig_AudioModelFallsBackToTheVoiceModel(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(`
extractors:
  audio:
    model_path: /models/whisper/ggml-small.bin
voice:
  stt:
    model: /models/whisper/ggml-base.en.bin
`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.AudioExtractionModel(); got != "/models/whisper/ggml-small.bin" {
		t.Fatalf("the audio key wins: %q", got)
	}
	cfg.Extractors.Audio.ModelPath = "  "
	if got := cfg.AudioExtractionModel(); got != "/models/whisper/ggml-base.en.bin" {
		t.Fatalf("unset falls back to voice.stt.model: %q", got)
	}
	cfg.Voice.STT.Model = ""
	if got := cfg.AudioExtractionModel(); got != "" {
		t.Fatalf("neither set: %q", got)
	}
	var nilCfg *Config
	if nilCfg.AudioExtractionModel() != "" {
		t.Fatal("a nil config has no model")
	}
}
