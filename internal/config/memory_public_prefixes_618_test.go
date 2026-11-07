package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #618 C2 (firewall design, "Amendment 2026-11-06 (#618)"): the daemon's
// memory.public_source_prefixes defaults to docs/public/, an explicit empty
// list derives nothing, and an invalid prefix refuses to load. Before the
// change the setting did not exist.
func TestMemoryPublicSourcePrefixes_618(t *testing.T) {
	if got := DefaultConfig().Memory.PublicSourcePrefixes; !reflect.DeepEqual(got, []string{"docs/public/"}) {
		t.Fatalf("default %v, want [docs/public/]", got)
	}
	for _, tc := range []struct {
		yaml string
		want []string
	}{
		{"memory:\n  enabled: true\n", []string{"docs/public/"}},
		{"memory:\n  public_source_prefixes: []\n", []string{}},
		{"memory:\n  public_source_prefixes: [docs/public/, docs/architect/]\n", []string{"docs/public/", "docs/architect/"}},
	} {
		c := DefaultConfig()
		if err := yaml.Unmarshal([]byte(tc.yaml), c); err != nil {
			t.Fatal(err)
		}
		if got := c.Memory.PublicSourcePrefixes; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.yaml, got, tc.want)
		}
	}
	for _, bad := range []string{"docs/public", "docs/%/", "docs/../legal/", ""} {
		c := DefaultConfig()
		c.API.AuthEnabled = false
		c.Memory.PublicSourcePrefixes = []string{"docs/public/", bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "memory.public_source_prefixes") {
			t.Errorf("prefix %q: want a load error naming memory.public_source_prefixes, got %v", bad, err)
		}
	}
}
