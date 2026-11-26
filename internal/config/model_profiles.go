package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelProfileDirName is the directory, under the configs root, that holds
// the provider profiles Vornik ships (GitHub #880, project and swarm registry
// design §11.2 R5).
const ModelProfileDirName = "model-profiles"

// ModelProfile maps model tiers, and a few "<swarm>/<role>" keys, to the
// model IDs one provider serves. Applying it writes Tiers and Roles into
// runtime.agent_llm; after that it is instance config.
type ModelProfile struct {
	// Name is the file name without .yaml; set by the loader.
	Name        string `yaml:"-"`
	Description string `yaml:"description"`
	// EndpointHosts are the hosts this profile's IDs are served from, as
	// shell-style patterns (path.Match). Setup offers the profile when the
	// validated endpoint's host matches one.
	EndpointHosts []string               `yaml:"endpoint_hosts"`
	Tiers         map[string]ModelChoice `yaml:"tiers"`
	Roles         map[string]ModelChoice `yaml:"roles"`
}

// LoadModelProfiles reads every <dir>/*.yaml, sorted by name. An unknown key
// is an error, so a typo cannot silently drop a mapping.
func LoadModelProfiles(dir string) ([]ModelProfile, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]ModelProfile, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var p ModelProfile
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("model profile %s: %w", filepath.Base(f), err)
		}
		p.Name = strings.TrimSuffix(filepath.Base(f), ".yaml")
		out = append(out, p)
	}
	return out, nil
}

// MatchModelProfile returns the profile whose EndpointHosts match endpoint's
// host, or nil. Only the host is compared, so a path or a look-alike suffix
// never matches.
func MatchModelProfile(profiles []ModelProfile, endpoint string) *ModelProfile {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for i := range profiles {
		for _, pat := range profiles[i].EndpointHosts {
			if ok, _ := path.Match(strings.ToLower(pat), host); ok {
				return &profiles[i]
			}
		}
	}
	return nil
}

// ApplyModelProfile returns content with runtime.agent_llm.tiers and
// runtime.agent_llm.roles replaced by p's maps, whole. Everything else in the
// file, comments included, is kept. A map the profile leaves empty is
// removed, so its entries inherit the setup model.
func ApplyModelProfile(content []byte, p ModelProfile) ([]byte, error) {
	out, err := ReplaceModelChoices(content, "runtime.agent_llm.tiers", p.Tiers)
	if err != nil {
		return nil, fmt.Errorf("model profile %s: %w", p.Name, err)
	}
	if out, err = ReplaceModelChoices(out, "runtime.agent_llm.roles", p.Roles); err != nil {
		return nil, fmt.Errorf("model profile %s: %w", p.Name, err)
	}
	return out, nil
}

// ReplaceModelChoices returns content with the map at dottedKey replaced by
// m, whole (an empty m removes it), keeping the rest of the file and its
// comments. Keys may not hold a dot, which would split the path.
func ReplaceModelChoices(content []byte, dottedKey string, m map[string]ModelChoice) ([]byte, error) {
	out, _, err := DeleteYAMLKey(content, dottedKey)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.Contains(k, ".") || k == "" {
			return nil, fmt.Errorf("key %q in %s cannot be empty or hold a dot", k, dottedKey)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := m[k]
		if out, _, err = SetYAMLKey(out, dottedKey+"."+k+".model", c.Model); err != nil {
			return nil, err
		}
		if c.Fallback != "" {
			if out, _, err = SetYAMLKey(out, dottedKey+"."+k+".fallback", c.Fallback); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
