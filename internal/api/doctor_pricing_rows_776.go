package api

// GitHub #776 (config-tree drift design, amendment 2026-11-19, change 3).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// pricingRowsNote adds to the pricing.yaml drift finding which shipped model
// ids the deployed file lacks, and whether config-deploy added rows on
// upgrade. It returns "" when either file cannot be read or parsed: the
// finding then says what it said before.
func pricingRowsNote(configDir string) string {
	deployedPath := filepath.Join(configDir, "pricing.yaml")
	deployed, err := pricingModelIDs(deployedPath)
	if err != nil {
		return ""
	}
	shipped, err := pricingModelIDs(filepath.Join(configDir, ".templates", "pricing.yaml"))
	if err != nil {
		return ""
	}
	var missing []string
	for id := range shipped {
		if !deployed[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	note := "; every shipped model id is present"
	if n := len(missing); n > 0 {
		shown := missing
		more := ""
		if n > 10 {
			shown, more = missing[:10], fmt.Sprintf(" and %d more", n-10)
		}
		note = fmt.Sprintf("; %d shipped model id(s) missing from the deployed file: %s%s (they are charged at the default rate)", n, strings.Join(shown, ", "), more)
	}
	if b, err := os.ReadFile(deployedPath); err == nil && strings.Contains(string(b), "# Added by config-deploy") {
		note += "; rows under its '# Added by config-deploy' lines were added on upgrade"
	}
	return note
}

func pricingModelIDs(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Models map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(f.Models))
	for id := range f.Models {
		ids[id] = true
	}
	return ids, nil
}
