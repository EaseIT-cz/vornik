package config

import (
	"slices"
	"strings"
)

// CurrentAdminKeysInFile returns the instance's current instance_admin keys
// by the config file as written (GitHub #582 commit security review, #581
// D3): each listed literally in both api.api_keys and admin.allowed_keys,
// and not hosting.previous_admin_api_key. A key held as an ${ENV} reference
// is not returned: a literal edit could not remove it. Order is api.api_keys'.
func CurrentAdminKeysInFile(content []byte) []string {
	allowed := GetYAMLStringList(content, "admin.allowed_keys")
	prev := GetYAMLString(content, "hosting.previous_admin_api_key")
	var out []string
	for _, k := range GetYAMLStringList(content, "api.api_keys") {
		if k == "" || strings.Contains(k, "${") || k == prev || !slices.Contains(allowed, k) || slices.Contains(out, k) {
			continue
		}
		out = append(out, k)
	}
	return out
}
