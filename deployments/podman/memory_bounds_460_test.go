package podman

import (
	"os"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// GitHub #460 item 3 (08-runtime-manager-podman.md S6, "every compose service
// and sidecar unit gets a memory bound"): a long-running sidecar with no
// memory bound can take the whole host (the 2026-10-14 OOM class). Every
// service of the six stacks declares a memory limit and an equal
// memswap_limit (no swap), and the two contrib units pass equal --memory and
// --memory-swap.
func TestCompose_EveryServiceHasANoSwapMemoryBound_460(t *testing.T) {
	// The stacks both editions ship; the Enterprise-only ones are in
	// memory_bounds_460_ee_test.go, which the Community export prunes with
	// them (the 2026.11.3 release CI failed the CE export on this test
	// reading scraper.compose.yaml, which the export removes).
	n := requireNoSwapMemoryBounds(t, []string{"deps.compose.yaml", "gateway.compose.yaml",
		"pagedrop.compose.yaml", "cluster.compose.yaml"}, []string{"../../contrib/systemd/vornik-gateway.service"})
	if n < 7 {
		t.Fatalf("examined %d services, want the 7 of the census in the shared stacks", n)
	}
}

// requireNoSwapMemoryBounds checks every service of the compose files and
// every unit, and returns how many services it examined.
func requireNoSwapMemoryBounds(t *testing.T, composeFiles, units []string) int {
	t.Helper()
	type service struct {
		MemLimit     string `yaml:"mem_limit"`
		MemswapLimit string `yaml:"memswap_limit"`
		Deploy       struct {
			Resources struct {
				Limits struct {
					Memory string `yaml:"memory"`
				} `yaml:"limits"`
			} `yaml:"resources"`
		} `yaml:"deploy"`
	}
	examined := 0
	for _, f := range composeFiles {
		var doc struct {
			Services map[string]service `yaml:"services"`
		}
		if err := yaml.Unmarshal([]byte(readCompose(t, f)), &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, s := range doc.Services {
			examined++
			limit := s.MemLimit
			if limit == "" {
				limit = s.Deploy.Resources.Limits.Memory
			}
			if limit == "" || s.MemswapLimit != limit {
				t.Errorf("%s: service %q has memory limit %q and memswap_limit %q; want a limit and an equal memswap_limit", f, name, limit, s.MemswapLimit)
			}
		}
	}
	re := regexp.MustCompile(`--memory=(\S+)[\s\\]+--memory-swap=(\S+)`)
	for _, u := range units {
		b, err := os.ReadFile(u)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindStringSubmatch(string(b))
		if m == nil || m[1] != m[2] {
			t.Errorf("%s: podman run must pass equal --memory and --memory-swap, got %v", u, m)
		}
	}
	t.Logf("memory-bound law: examined %d compose services in %d files and %d units", examined, len(composeFiles), len(units))
	return examined
}
