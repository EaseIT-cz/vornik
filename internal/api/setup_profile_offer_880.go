package api

// After a chat commit, setup names the shipped provider profile that matches
// the saved endpoint (GitHub #880, project and swarm registry design §11.2
// R6). Setup writes no tier itself: applying stays on the one audited path,
// POST /api/v1/models/profiles/{name}/apply.

import (
	"html"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/config"
)

type setupProfileOfferDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Command     string `json:"command"`
}

func (s *Server) setupProfileOffer(endpoint string) *setupProfileOfferDTO {
	if strings.TrimSpace(s.configsDir) == "" {
		return nil
	}
	profiles, err := config.LoadModelProfiles(filepath.Join(s.configsDir, config.ModelProfileDirName))
	if err != nil {
		return nil
	}
	p := config.MatchModelProfile(profiles, endpoint)
	if p == nil {
		return nil
	}
	return &setupProfileOfferDTO{Name: p.Name, Description: p.Description,
		Command: "vornikctl models profile apply " + p.Name + " --apply"}
}

func setupProfileOfferHTML(o *setupProfileOfferDTO) string {
	if o == nil {
		return ""
	}
	return `<div class="mt-2 text-gray-300">Provider profile <span class="font-semibold">` + html.EscapeString(o.Name) +
		`</span> matches this endpoint and maps each role's model tier to a tuned model: ` + html.EscapeString(o.Description) +
		` Apply it with <code>` + html.EscapeString(o.Command) + `</code>, or skip it and every role runs on this model.</div>`
}
