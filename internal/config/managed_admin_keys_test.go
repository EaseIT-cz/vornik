package config

import (
	"slices"
	"testing"
)

// GitHub #581 D3 and #582: the instance's current instance_admin keys, read
// from the config file as written. One implementation serves the credential
// operations (is the presented key current?) and the idle gate (which key do
// I use?).
func TestCurrentAdminKeysInFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{"one key in both lists", "api:\n  api_keys: [k1]\nadmin:\n  allowed_keys: [k1]\n", []string{"k1"}},
		{"in one list only", "api:\n  api_keys: [k1, k2]\nadmin:\n  allowed_keys: [k1]\n", []string{"k1"}},
		{"during a rotation the previous key is not current",
			"api:\n  api_keys: [old, new]\nadmin:\n  allowed_keys: [old, new]\nhosting:\n  previous_admin_api_key: old\n", []string{"new"}},
		{"an env reference is not a literal key", "api:\n  api_keys: [\"${K}\"]\nadmin:\n  allowed_keys: [\"${K}\"]\n", nil},
		{"no lists", "hosting:\n  mode: managed\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CurrentAdminKeysInFile([]byte(tc.yaml)); !slices.Equal(got, tc.want) {
				t.Errorf("CurrentAdminKeysInFile = %q; want %q", got, tc.want)
			}
		})
	}
}
