package api

// GitHub #880, project and swarm registry design §11.2 R6: after a chat
// commit, setup names the shipped profile whose endpoint_hosts match the
// saved endpoint, with the command that applies it, and writes no tier.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupProfileOffer_880(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "model-profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-profiles", "ollama-cloud.yaml"), []byte(profileYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{configsDir: dir}
	offer := s.setupProfileOffer("https://ollama.com/v1")
	if offer == nil || offer.Name != "ollama-cloud" || !strings.Contains(offer.Command, "vornikctl models profile apply ollama-cloud --apply") {
		t.Fatalf("offer = %+v", offer)
	}
	if html := setupProfileOfferHTML(offer); !strings.Contains(html, "ollama-cloud") || !strings.Contains(html, "--apply") {
		t.Fatalf("html = %s", html)
	}
	for _, endpoint := range []string{"https://openrouter.ai/api/v1", "", "http://192.0.2.10:8012/v1"} {
		if o := s.setupProfileOffer(endpoint); o != nil {
			t.Errorf("%q: offered %+v, want nothing", endpoint, o)
		}
	}
	if o := (&Server{}).setupProfileOffer("https://ollama.com/v1"); o != nil {
		t.Errorf("no configs dir: offered %+v", o)
	}
	if setupProfileOfferHTML(nil) != "" {
		t.Error("no offer must render nothing")
	}
}
