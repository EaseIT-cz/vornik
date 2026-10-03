package probe

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// voice-messages-design.md §8 (incident 2026-10-03): the decode probe
// judges the WAV the way the STT pipeline does before whisper-cli runs.
// "Longer than 44 bytes and starts RIFF" passed a WAV with no audio frames.
func TestRun_DecodeProbeRefusesAWAVWithNoFrames(t *testing.T) {
	headerOnly := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"),
		[]byte("trailing-bytes-past-the-empty-data-chunk")...)
	base := fakeTools(t, "")
	sb := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
		if spec.Entrypoint == "ffmpeg" && spec.Feature == sandboxtool.FeatureVoiceSTT {
			return os.WriteFile(filepath.Join(out, "audio.wav"), headerOnly, 0o600)
		}
		res, err := base.Run(context.Background(), spec)
		if err != nil {
			return err
		}
		defer res.Close()
		entries, err := os.ReadDir(res.OutDir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			b, rerr := os.ReadFile(filepath.Join(res.OutDir, e.Name()))
			if rerr != nil {
				return rerr
			}
			if werr := os.WriteFile(filepath.Join(out, e.Name()), b, 0o600); werr != nil {
				return werr
			}
		}
		return nil
	})
	got := byTool(Run(context.Background(), sb, models(t)))
	for _, name := range []string{"ffmpeg opus", "ffmpeg aac"} {
		if got[name].Status == StatusOK {
			t.Errorf("%s passed a WAV with no audio frames: %+v", name, got[name])
		}
	}
}
