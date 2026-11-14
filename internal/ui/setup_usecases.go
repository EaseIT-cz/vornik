package ui

// SetupUseCase is one card of the first-run page's step 2, "Choose what it
// is for" (installation-onboarding-guide-design.md, "Amendment 2026-11-14
// (#758)", W2): the use cases the design lists, each backed by a project
// template. Choosing one makes its project the dispatcher, creating it from
// the template first when it is not loaded.
type SetupUseCase struct {
	Key, Title, Body string
	Slug             string // the project template
	ProjectID        string // the project it creates or reuses
	DisplayName      string
	Loaded           bool   // the project is already in the registry
	ButtonLabel      string // "Choose", or "Chosen: apply again" for the current dispatcher
}

// setupUseCases is the design's list, in its order (§ Primary Use-Case
// Decision): generic assistant, then companion. The automated-development
// template does not exist yet (Slice 3), so it is not offered.
func (s *Server) setupUseCases(dispatcher string) []SetupUseCase {
	cases := []SetupUseCase{
		{Key: "assistant", Title: "A personal assistant", Body: "Answers questions, drafts text, keeps reminders and runs small jobs for you.", Slug: "personal-assistant", ProjectID: "assistant", DisplayName: "Personal Assistant"},
		{Key: "companion", Title: "A companion for your coding agent", Body: "Gives Claude Code or Codex long-term memory and background reviews.", Slug: "companion", ProjectID: "companion", DisplayName: "Companion"},
	}
	for i := range cases {
		cases[i].Loaded = s.projectReg != nil && s.projectReg.GetProject(cases[i].ProjectID) != nil
		cases[i].ButtonLabel = "Choose"
		if dispatcher != "" && dispatcher == cases[i].ProjectID {
			cases[i].ButtonLabel = "Chosen: apply again"
		}
	}
	return cases
}
