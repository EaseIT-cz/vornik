package agentadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/agentns"
)

// UpdateProjectInput edits descriptive metadata, never project identity/reach.
// RawMessage distinguishes an omitted field from an explicit null.
type UpdateProjectInput struct {
	Project     string          `json:"project"`
	Purpose     json.RawMessage `json:"purpose,omitempty"`
	DisplayName json.RawMessage `json:"display_name,omitempty"`
}

func (r *Renderer) updateProject(st *State, raw json.RawMessage) Change {
	const verb = VerbUpdateProject
	var in UpdateProjectInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, st.Namespace, "%v", err)
	}
	in.Project = projectSlug(st.Namespace, in.Project)
	if err := checkSlug("project", in.Project); err != nil {
		return refuse(verb, st.Namespace, "%v", err)
	}
	pid := agentns.ID(st.Namespace, in.Project)
	if st.Projects[pid] == nil {
		return refuse(verb, st.Namespace, "there is no project %q", pid)
	}
	if len(in.Purpose) == 0 && len(in.DisplayName) == 0 {
		return refuse(verb, st.Namespace, "supply purpose or display_name")
	}
	updates := map[string]string{}
	for _, field := range []struct {
		name, key string
		raw       json.RawMessage
	}{{"purpose", "description", in.Purpose}, {"display_name", "displayName", in.DisplayName}} {
		if len(field.raw) == 0 {
			continue
		}
		var value string
		if err := json.Unmarshal(field.raw, &value); err != nil {
			return refuse(verb, st.Namespace, "%s must be a string", field.name)
		}
		if err := checkOneLine(field.name, value, true); err != nil {
			return refuse(verb, st.Namespace, "%v", err)
		}
		updates[field.key] = value
	}
	path := projectPath(pid)
	source, ok := st.ProjectYAML[path]
	if !ok || HashContent(source) != st.FileHashes[path] {
		return refuse(verb, st.Namespace, "project source is missing or changed; reload the setup and retry")
	}
	content, err := patchProjectMetadata(source, updates)
	if err != nil {
		return refuse(verb, st.Namespace, "project source cannot be edited: %v", err)
	}
	c := Change{Class: Inert, Ops: []FileOp{{Op: OpReplace, Path: path, Content: content}}, ReadSet: map[string]string{}, Locks: []string{lockProject(pid), path}}
	expect(st, c.ReadSet, path)
	c.Sentence = fmt.Sprintf("Your assistant (%s) updated the name or purpose of project %q.", st.Namespace, pid)
	return c
}

// Patch the original mapping instead of re-rendering a template: metadata
// edits must preserve operator settings and comments outside these fields.
func patchProjectMetadata(source []byte, updates map[string]string) (string, error) {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(&doc); err != nil {
		return "", err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("expected exactly one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("expected one project mapping")
	}
	mapping := doc.Content[0]
	for _, key := range []string{"description", "displayName"} {
		value, ok := updates[key]
		if !ok {
			continue
		}
		found := false
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				node := mapping.Content[i+1]
				if node.Anchor != "" {
					return "", fmt.Errorf("metadata field %s has an anchor; editing it could change other settings", key)
				}
				node.Kind, node.Tag, node.Value, node.Style, node.Content = yaml.ScalarNode, "!!str", value, yaml.DoubleQuotedStyle, nil
				found = true
				break
			}
		}
		if !found {
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle})
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return out.String(), nil
}
