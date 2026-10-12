package agentadmin

import "encoding/json"

// VerbShowWorkflow reads an owned broker workflow's active definition.
const VerbShowWorkflow = "show_workflow"

// ShowWorkflowInput reads an active definition, scoped by the caller's key.
type ShowWorkflowInput struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
}

// ParseShowWorkflowInput validates and namespace-normalizes a show_workflow request.
func ParseShowWorkflowInput(ns string, raw json.RawMessage) (ShowWorkflowInput, error) {
	var in ShowWorkflowInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	in.Project = projectSlug(ns, in.Project)
	if err := checkSlug("project", in.Project); err != nil {
		return in, err
	}
	if err := checkSlug("slug", in.Slug); err != nil {
		return in, err
	}
	return in, nil
}
