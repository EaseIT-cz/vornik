package cli

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// newCLIRegistry is a registry for a command that loads the deployed tree
// without a config already in hand (init project, init swarm). The registry
// fails closed on agent role models (agent-administered design §18.6 item
// 2, review 20261003-a525 A1), so it is given agent_admin.models from the
// config.yaml the daemon reads: VORNIK_CONFIG, else the one beside the
// configs directory. With none found the catalogue stays empty and an agent
// role naming a model is refused, as the daemon with no catalogue would.
// Only the agent_admin block is read: a full config load validates keys
// (credentials, auth) an offline command may not have, and touching it
// here would make an unrelated validation failure refuse an agent role.
func newCLIRegistry(configsDir string) *registry.Registry {
	reg := registry.New()
	for _, path := range []string{os.Getenv("VORNIK_CONFIG"), filepath.Join(filepath.Dir(configsDir), "config.yaml")} {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			AgentAdmin config.AgentAdminConfig `yaml:"agent_admin"`
		}
		if yaml.Unmarshal(raw, &doc) == nil {
			reg.SetAgentModelCatalogue(doc.AgentAdmin.ModelIDs())
			break
		}
	}
	return reg
}
