package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/storage"
)

// Process-spawn law S5b: the media extractors run through the daemon's
// sandbox runner, and audio extraction gets its whisper model from
// extractors.audio.model_path, falling back to voice.stt.model (design §7.1
// decision 3). With no runner, every media extractor reports "not
// available" — it never reaches for a host binary.
func TestExtractorRegistry_MediaExtractorsUseTheSandbox(t *testing.T) {
	model := filepath.Join(t.TempDir(), "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("ggml"), 0o600); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(t.TempDir(), "clip")
	if err := os.WriteFile(media, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := newConflictTestDB(t)
	registry := func(cfg *config.Config) *extractor.Registry {
		cfg.Storage.ArtifactsPath = t.TempDir()
		c := &Container{Logger: zerolog.Nop(), Config: cfg,
			repos: &storage.Repositories{ExtractedDocuments: sqlite.NewExtractedDocumentRepository(db.DB)}}
		reg := c.ExtractorRegistry()
		if reg == nil {
			t.Fatal("registry not built")
		}
		return reg
	}
	extract := func(reg *extractor.Registry, mime string) error {
		ext, err := reg.For(mime)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ext.Extract(context.Background(), extractor.Source{FilePath: media, MimeType: mime})
		return err
	}

	// The voice model is the fallback: the error is about the runner, not
	// about a missing model.
	reg := registry(&config.Config{Voice: config.VoiceConfig{STT: config.VoiceSTTConfig{Model: model}}})
	err := extract(reg, "audio/mpeg")
	if !errors.Is(err, sandboxtool.ErrNotAvailable) || !strings.Contains(err.Error(), "no sandbox runner") {
		t.Fatalf("audio with the voice model: %v", err)
	}
	for _, mime := range []string{"application/pdf", "video/mp4"} {
		if err := extract(reg, mime); !errors.Is(err, sandboxtool.ErrNotAvailable) {
			t.Errorf("%s with no runner: want not available, got %v", mime, err)
		}
	}

	// Neither key set: audio extraction names the key to set.
	reg = registry(&config.Config{})
	if err := extract(reg, "audio/mpeg"); !errors.Is(err, sandboxtool.ErrNotAvailable) ||
		!strings.Contains(err.Error(), "extractors.audio.model_path") {
		t.Fatalf("audio with no model: %v", err)
	}
}
