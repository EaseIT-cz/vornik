// Package statusfmt is a deliberately BROKEN variant (drops the checks from the JSON) for sw-01-flag's acceptance suite.
package statusfmt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func RenderStatus(name string, healthy bool, checks map[string]bool) string {
	state := "UNHEALTHY"
	if healthy {
		state = "HEALTHY"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", name, state)
	keys := make([]string, 0, len(checks))
	for k := range checks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %v\n", k, checks[k])
	}
	return b.String()
}

func RenderStatusJSON(name string, healthy bool, checks map[string]bool) string {
	if checks == nil {
		checks = map[string]bool{}
	}
	out, _ := json.Marshal(struct {
		Name    string          `json:"name"`
		Healthy bool            `json:"healthy"`
		Checks  map[string]bool `json:"checks"`
	}{name, healthy, map[string]bool{}})
	return string(out)
}
