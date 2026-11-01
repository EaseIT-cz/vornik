package api

// Template seeding in managed setup (GitHub #587,
// operations-api-contract-design.md §7.2.1): the templates vornik-cloud's
// customer chose at sign-up are seeded as fixed projects in the setup commit.
// Setup never overwrites or deletes a project: a template's project is
// current once its projects/<id>.yaml exists, and a missing file of a
// partial project is written while the files that exist are kept.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/templates"
)

// managedSeed is one sign-up template: the shipped template it renders and
// the fixed project id it seeds.
type managedSeed struct {
	choice, template, project string
	dispatcher                bool // the dispatcher project is set to it
}

// managedSeeds is the closed list of sign-up templates. "Engineering memory"
// is the companion; the personal assistant also answers the web chat.
var managedSeeds = []managedSeed{
	{choice: "engineering", template: "companion", project: "companion"},
	{choice: "assistant", template: "personal-assistant", project: "assistant", dispatcher: true},
}

// managedSeedsFor returns the seeds a request names; "" when every name is
// known and none repeats, else the refusal.
func managedSeedsFor(names []string) ([]managedSeed, string) {
	var out []managedSeed
	seen := map[string]bool{}
	for _, n := range names {
		var found *managedSeed
		for i := range managedSeeds {
			if managedSeeds[i].choice == n {
				found = &managedSeeds[i]
			}
		}
		if found == nil || seen[n] {
			return nil, `templates is a list of distinct names from "engineering", "assistant"`
		}
		seen[n] = true
		out = append(out, *found)
	}
	return out, ""
}

// managedDispatcher is the dispatcher project the seeds set, "" for none.
func managedDispatcher(seeds []managedSeed) string {
	for _, sd := range seeds {
		if sd.dispatcher {
			return sd.project
		}
	}
	return ""
}

// missingSeeds returns the seeds whose project file does not exist yet.
func (s *Server) missingSeeds(seeds []managedSeed) []managedSeed {
	var out []managedSeed
	for _, sd := range seeds {
		if _, err := os.Stat(filepath.Join(s.configsDir, "projects", sd.project+".yaml")); errors.Is(err, os.ErrNotExist) {
			out = append(out, sd)
		}
	}
	return out
}

// seedProjects renders each seed and writes the files of it that do not
// exist, keeping those that do. It returns the projects written.
func (s *Server) seedProjects(seeds []managedSeed) ([]string, error) {
	seeded := []string{}
	for _, sd := range seeds {
		m, ok := s.projectTemplates.Get(sd.template)
		if !ok {
			return seeded, fmt.Errorf("template %q is not shipped", sd.template)
		}
		rendered, err := s.projectTemplates.MaterialiseFiles(m, map[string]string{"projectId": sd.project})
		if err != nil {
			return seeded, fmt.Errorf("template %q: %w", sd.template, err)
		}
		for target := range rendered {
			if _, err := os.Stat(filepath.Join(s.configsDir, target)); err == nil {
				delete(rendered, target)
			}
		}
		if _, err := templates.WriteRenderedFilesExclusive(s.configsDir, rendered); err != nil {
			return seeded, fmt.Errorf("template %q: %w", sd.template, err)
		}
		seeded = append(seeded, sd.project)
	}
	return seeded, nil
}
