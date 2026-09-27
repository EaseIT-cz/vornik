package service

import (
	"os"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/voice"
)

// initVoice constructs the daemon-level speech-to-text and
// text-to-speech providers from config.Voice. Result lands on
// c.voiceSTT / c.voiceTTS; either may stay nil when the
// corresponding sub-block is empty or names an unsupported
// provider. Construction failures from a well-formed config
// (e.g. an unsupported provider name) log a warning and leave
// the provider nil — voice is opt-in scaffolding and a misconfig
// should not block daemon boot. Truly broken provider configs
// (e.g. whisper-local without a model path) surface as a returned
// error so the operator sees the typo loudly.
//
// The providers run whisper-cli, piper and ffmpeg in the pinned agent
// image through the sandbox runner (process-spawn law S5b,
// https://docs.vornik.io §7), so
// there is no host binary to probe: boot checks the model files (a stat)
// and warns about the host-path keys that no longer do anything. Whether
// the image carries the tools is logged by initSandboxTools, and
// `vornikctl doctor` runs each one.
func (c *Container) initVoice() error {
	// STT --------------------------------------------------------
	sttRaw := strings.TrimSpace(c.Config.Voice.STT.Provider)
	if sttRaw == "" {
		c.Logger.Debug().Msg("voice: STT disabled (no provider configured)")
	} else {
		c.Logger.Info().
			Str("provider", sttRaw).
			Str("model", c.Config.Voice.STT.Model).
			Str("language_hint", c.Config.Voice.STT.LanguageHint).
			Msg("voice: configuring STT provider")
	}
	stt, err := buildSTTProvider(c.Config.Voice.STT, c.sandbox())
	if err != nil {
		return err
	}
	if stt == nil && sttRaw != "" {
		c.Logger.Warn().
			Str("provider", sttRaw).
			Msg("voice: unsupported STT provider — voice inbound disabled (supported: whisper-local)")
	}
	if stt != nil {
		probeSTT(c, c.Config.Voice.STT)
	}
	c.voiceSTT = stt

	// TTS --------------------------------------------------------
	ttsRaw := strings.TrimSpace(c.Config.Voice.TTS.Provider)
	if ttsRaw == "" {
		c.Logger.Debug().Msg("voice: TTS disabled (no provider configured)")
	} else {
		c.Logger.Info().
			Str("provider", ttsRaw).
			Str("voice_model", c.Config.Voice.TTS.Voice).
			Float64("speed", c.Config.Voice.TTS.Speed).
			Int("max_text_runes", c.Config.Voice.TTS.MaxTextRunes).
			Msg("voice: configuring TTS provider")
	}
	tts, err := buildTTSProvider(c.Config.Voice.TTS, c.sandbox())
	if err != nil {
		return err
	}
	if tts == nil && ttsRaw != "" {
		c.Logger.Warn().
			Str("provider", ttsRaw).
			Msg("voice: unsupported TTS provider — voice outbound disabled (supported: piper)")
	}
	if tts != nil {
		probeTTS(c, c.Config.Voice.TTS)
	}
	c.voiceTTS = tts

	if c.voiceSTT != nil || c.voiceTTS != nil {
		c.Logger.Info().
			Bool("stt", c.voiceSTT != nil).
			Bool("tts", c.voiceTTS != nil).
			Msg("voice providers initialized")
	}
	return nil
}

// sandbox is the runner as the features take it: a nil runner is a nil
// Sandbox (never a typed nil), which the features report as "not
// available".
func (c *Container) sandbox() sandboxtool.Sandbox {
	if c.sandboxRunner == nil {
		return nil
	}
	return c.sandboxRunner
}

func buildSTTProvider(cfg config.VoiceSTTConfig, sb sandboxtool.Sandbox) (voice.STTProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return nil, nil
	case "whisper-local":
		return voice.NewWhisperLocalSTT(voice.WhisperConfig{
			ModelPath:    strings.TrimSpace(cfg.Model),
			LanguageHint: cfg.LanguageHint,
			Threads:      cfg.Threads,
			Sandbox:      sb,
		})
	default:
		return nil, nil
	}
}

func buildTTSProvider(cfg config.VoiceTTSConfig, sb sandboxtool.Sandbox) (voice.TTSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return nil, nil
	case "piper":
		return voice.NewPiperLocalTTS(voice.PiperConfig{
			ModelPath:    strings.TrimSpace(cfg.Voice),
			DefaultSpeed: cfg.Speed,
			MaxTextRunes: cfg.MaxTextRunes,
			Sandbox:      sb,
		})
	default:
		return nil, nil
	}
}

// probeSTT checks the STT model on the host and warns about the host-path
// keys that are ignored since the tools moved into the agent image. It
// doesn't fail boot: the operator sees the warnings before the first voice
// message gets a humane error reply.
func probeSTT(c *Container, cfg config.VoiceSTTConfig) {
	probeModel(c, "whisper", cfg.Model)
	warnIgnoredPath(c, "voice.stt.binary_path", cfg.BinaryPath)
	warnIgnoredPath(c, "voice.stt.ffmpeg_path", cfg.FFmpegPath)
}

func probeTTS(c *Container, cfg config.VoiceTTSConfig) {
	probeModel(c, "piper", cfg.Voice)
	if v := strings.TrimSpace(cfg.Voice); v != "" {
		if _, err := os.Stat(v + ".json"); err != nil {
			c.Logger.Warn().Str("path", v+".json").
				Msg("voice: piper voice config not found beside the model — piper needs <voice>.onnx.json next to the .onnx")
		}
	}
	warnIgnoredPath(c, "voice.tts.binary_path", cfg.BinaryPath)
	warnIgnoredPath(c, "voice.tts.ffmpeg_path", cfg.FFmpegPath)
}

// warnIgnoredPath warns when a host-binary key is still set: since 2026.9.7
// the voice tools run in the agent image and the key does nothing.
func warnIgnoredPath(c *Container, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	c.Logger.Warn().Str("key", key).Str("value", value).
		Msgf("voice: %s is ignored — the voice tools run in the agent image (process-spawn law S5b); remove the key", key)
}

// probeModel stat's the model file and logs its size. Empty path
// is a configuration error (provider construction would have
// failed already) — the wrapper still reports it for symmetry.
func probeModel(c *Container, label, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		c.Logger.Warn().
			Msgf("voice: %s model path is empty", label)
		return
	}
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		c.Logger.Warn().
			Str("path", path).
			Msgf("voice: %s model path is a directory, not a file", label)
	case err == nil:
		c.Logger.Info().
			Str("path", path).
			Int64("size_bytes", info.Size()).
			Msgf("voice: %s model OK", label)
	case os.IsNotExist(err):
		c.Logger.Warn().
			Str("path", path).
			Msgf("voice: %s model file not found — first voice call will fail", label)
	default:
		c.Logger.Warn().
			Err(err).
			Str("path", path).
			Msgf("voice: %s model stat failed", label)
	}
}
