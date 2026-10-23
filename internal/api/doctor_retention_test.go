package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// The warning must name the article and the consequence, not just report a flag.
// An operator reading "retention.enabled=false" learns nothing; the point is
// that the deployment keeps personal data forever and that this is an Art 5(1)(e)
// problem only they can close.
func TestCheckRetentionEnabled_OffWarnsWithTheArticle(t *testing.T) {
	h := &DoctorHandlers{retentionKnown: true, retentionEnabled: false}
	got := h.checkRetentionEnabled()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING", got.Status)
	}
	for _, want := range []string{"Art 5(1)(e)", "INDEFINITELY", "retention-recommended.yaml"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message missing %q: %s", want, got.Message)
		}
	}
}

// Enabled with no windows prunes nothing while LOOKING configured in a config
// diff — the same exposure as being off, with none of the visibility.
func TestCheckRetentionEnabled_EnabledButUnconfiguredWarns(t *testing.T) {
	h := &DoctorHandlers{retentionKnown: true, retentionEnabled: true}
	got := h.checkRetentionEnabled()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING", got.Status)
	}
	if !strings.Contains(got.Message, "prunes nothing") {
		t.Errorf("message should name the real consequence: %s", got.Message)
	}
}

// Memory chunks are the longest-lived personal data in the system and the row
// most often missed, so an otherwise-configured sweeper that omits it still
// warns rather than reporting healthy.
func TestCheckRetentionEnabled_MissingMemoryWindowWarns(t *testing.T) {
	h := &DoctorHandlers{
		retentionKnown: true, retentionEnabled: true,
		retentionWindows: map[string]int{"tasks_days": 365, "artifacts_days": 180},
	}
	got := h.checkRetentionEnabled()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING", got.Status)
	}
	if !strings.Contains(got.Message, "memory_chunks_days is UNSET") {
		t.Errorf("message should name the missing window: %s", got.Message)
	}
}

func TestCheckRetentionEnabled_FullyConfiguredIsOK(t *testing.T) {
	h := &DoctorHandlers{
		retentionKnown: true, retentionEnabled: true,
		retentionWindows: map[string]int{"tasks_days": 365, "memory_chunks_days": 730},
	}
	got := h.checkRetentionEnabled()
	if got.Status != "OK" {
		t.Fatalf("status = %q (%s), want OK", got.Status, got.Message)
	}
	// The message must show WHAT is configured, so a reviewer can sanity-check
	// the windows without opening config.yaml.
	if !strings.Contains(got.Message, "memory_chunks_days=730d") {
		t.Errorf("message should list the windows: %s", got.Message)
	}
}

// Without config wired the check must SKIP rather than warn: reporting an
// Art 5(1)(e) failure because the doctor was constructed without config would be
// a false alarm, and false alarms are how real ones get ignored.
func TestCheckRetentionEnabled_UnwiredSkips(t *testing.T) {
	got := (&DoctorHandlers{}).checkRetentionEnabled()
	if got.Status != "SKIPPED" {
		t.Errorf("status = %q, want SKIPPED", got.Status)
	}
}

// GitHub #115 status audit (2026-10-21, GDPR design §4.20): docs/legal/ropa.md
// says the doctor warns when embedding_cache_days is unset, and the Art 17
// redact design §4.4a calls that window not optional, because recall queries
// cached under their own hash are reached by no erasure path. The check never
// looked at it, so a sweeper with memory_chunks_days set reported OK while the
// embedding cache was kept for ever.
func TestCheckRetentionEnabled_MissingEmbeddingCacheWindowWarns(t *testing.T) {
	h := &DoctorHandlers{
		retentionKnown: true, retentionEnabled: true, embeddingCacheApplies: true,
		retentionWindows: map[string]int{"tasks_days": 365, "memory_chunks_days": 730},
	}
	got := h.checkRetentionEnabled()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q (%s), want WARNING", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "embedding_cache_days is UNSET") {
		t.Errorf("message should name the missing window: %s", got.Message)
	}
}

// On SQLite no embedder runs and the table does not exist, so the window does
// not apply and its absence is not a finding.
func TestCheckRetentionEnabled_EmbeddingCacheWindowNotRequiredOnSQLite(t *testing.T) {
	h := &DoctorHandlers{
		retentionKnown: true, retentionEnabled: true, embeddingCacheApplies: false,
		retentionWindows: map[string]int{"memory_chunks_days": 730},
	}
	if got := h.checkRetentionEnabled(); got.Status != "OK" {
		t.Fatalf("status = %q (%s), want OK", got.Status, got.Message)
	}
}

// The window applies only where the cache is written: the cache switched on,
// and a driver other than SQLite (empty means postgres). Review round 1 of
// §4.20: a Postgres deployment with the cache off holds no cached rows, so a
// warning there would be false.
func TestSetServerConfig_EmbeddingCacheWindowAppliesOnlyWhereTheCacheIsWritten(t *testing.T) {
	cases := []struct {
		driver  string
		enabled bool
		want    bool
	}{
		{"", true, true}, {"postgres", true, true}, {"sqlite", true, false},
		{"postgres", false, false}, {"", false, false},
	}
	for _, tc := range cases {
		cfg := &config.Config{}
		cfg.Database.Driver = tc.driver
		cfg.Memory.EmbeddingCacheEnabled = tc.enabled
		cfg.Retention.EmbeddingCacheDays = 30
		h := NewDoctorHandlers(nil)
		h.SetServerConfig(cfg)
		if h.embeddingCacheApplies != tc.want {
			t.Errorf("driver %q cache=%v: embeddingCacheApplies = %v, want %v",
				tc.driver, tc.enabled, h.embeddingCacheApplies, tc.want)
		}
		if h.retentionWindows["embedding_cache_days"] != 30 {
			t.Errorf("driver %q: embedding_cache_days must be recorded as a configured window", tc.driver)
		}
	}
}
