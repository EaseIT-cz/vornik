package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped retention profile is the file the doctor's Art 5(1)(e) warnings
// tell an operator to copy. A key misspelt in it would be ignored by the loader
// and leave that window unset, so every key must decode into RetentionConfig.
// GitHub #115 status audit (2026-10-21, GDPR design §4.20) added
// embedding_cache_days to the profile, the window the doctor now warns about.
func TestRetentionRecommendedProfile_EveryKeyIsARetentionField(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "configs", "retention-recommended.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	var doc struct {
		Retention RetentionConfig `yaml:"retention"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the profile must decode strictly into RetentionConfig: %v", err)
	}
	if !doc.Retention.Enabled {
		t.Error("the profile is the switched-on starting point")
	}
	if doc.Retention.MemoryChunksDays <= 0 || doc.Retention.EmbeddingCacheDays <= 0 {
		t.Errorf("the two windows the doctor warns about must be set: memory_chunks_days=%d embedding_cache_days=%d",
			doc.Retention.MemoryChunksDays, doc.Retention.EmbeddingCacheDays)
	}
}
