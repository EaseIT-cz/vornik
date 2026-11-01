package config

// GitHub #582 P4 (operations-api-contract-design.md §6.2, §6.3 F3): the two
// overlap slots of the operations credentials. Found at: design review (6.1
// B5: both credentials are read once and nothing accepts two side by side).

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

func TestRotation582_PreviousAdminAPIKeyMustAuthenticate(t *testing.T) {
	c := managedConfig()
	c.Hosting.PreviousAdminAPIKey = "sk-instance-admin"
	c.API.APIKeys = append(c.API.APIKeys, "sk-new-admin")
	c.Admin.AllowedKeys = append(c.Admin.AllowedKeys, "sk-new-admin")
	if err := c.Validate(); err != nil {
		t.Fatalf("an overlap with both keys listed must load: %v", err)
	}
	for _, mut := range []struct {
		name string
		f    func(*Config)
	}{
		{"listed nowhere", func(c *Config) { c.Hosting.PreviousAdminAPIKey = "sk-elsewhere" }},
		{"not in admin.allowed_keys", func(c *Config) { c.Hosting.PreviousAdminAPIKey = "sk-plain-key" }},
	} {
		c := managedConfig()
		mut.f(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "hosting.previous_admin_api_key") {
			t.Errorf("%s: err = %v, want a hosting.previous_admin_api_key refusal", mut.name, err)
		}
	}
	// The previous key may not be the only admin key: something must be current.
	c = managedConfig()
	c.Hosting.PreviousAdminAPIKey = "sk-instance-admin"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.previous_admin_api_key") {
		t.Errorf("a previous key with no current one: err = %v", err)
	}
	// Self mode has no instance_admin: the setting is refused there.
	s := baseValidConfig()
	s.Hosting.PreviousAdminAPIKey = "sk-x"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.previous_admin_api_key") {
		t.Errorf("self mode: err = %v", err)
	}
}

func TestRotation582_SlotsAreRedacted(t *testing.T) {
	c := managedConfig()
	c.Hosting.PreviousAdminAPIKey = "sk-previous-582"
	c.Metrics.PreviousScrapeToken = "scrape-previous-582"
	// The config dump's own path: JSON, a generic map, then RedactConfig
	// (internal/api/config_show_handler.go).
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(secrets.RedactConfig(generic))
	if err != nil {
		t.Fatal(err)
	}
	dump := string(out)
	for _, v := range []string{"sk-previous-582", "scrape-previous-582"} {
		if strings.Contains(dump, v) {
			t.Errorf("the config dump shows %q", v)
		}
	}
}

func TestRotation582_GetYAMLStringList(t *testing.T) {
	doc := []byte("api:\n  api_keys:\n    - ${VORNIK_A}\n    - sk-b\nadmin:\n  enabled: true\n")
	got := GetYAMLStringList(doc, "api.api_keys")
	if strings.Join(got, ",") != "${VORNIK_A},sk-b" {
		t.Errorf("got %q, want the raw values, env references unexpanded", got)
	}
	if GetYAMLStringList(doc, "admin.allowed_keys") != nil || GetYAMLStringList(doc, "admin.enabled") != nil {
		t.Error("a missing key or a scalar must give nil")
	}
}
