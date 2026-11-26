package agentadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/agentns"
)

// UpdateProjectInput edits descriptive metadata, never project identity/reach.
// RawMessage distinguishes an omitted field from an explicit null.
type UpdateProjectInput struct {
	Project     string          `json:"project"`
	Purpose     json.RawMessage `json:"purpose,omitempty"`
	DisplayName json.RawMessage `json:"display_name,omitempty"`
	// MaxConcurrentTasks sets maxConcurrentTasks, 1 to
	// agent_admin.max_concurrent_tasks (part 06 §6.9, GitHub #798).
	MaxConcurrentTasks json.RawMessage `json:"max_concurrent_tasks,omitempty"`
	// SharedMemory turns the namespace's shared memory on (an approval) or
	// off (applies at once) (part 24 §24.6 S3).
	SharedMemory *bool `json:"shared_memory,omitempty"`
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
	if len(in.Purpose) == 0 && len(in.DisplayName) == 0 && len(in.MaxConcurrentTasks) == 0 && in.SharedMemory == nil {
		return refuse(verb, st.Namespace, "supply purpose, display_name, max_concurrent_tasks or shared_memory")
	}
	if in.SharedMemory != nil && pid == agentns.ID(st.Namespace, "home") {
		return refuse(verb, st.Namespace, "the home project holds the shared memory; set shared_memory on the projects that share it")
	}
	p := st.Projects[pid]
	concurrency := 0
	if len(in.MaxConcurrentTasks) > 0 {
		if err := json.Unmarshal(in.MaxConcurrentTasks, &concurrency); err != nil || concurrency < 1 || concurrency > st.MaxConcurrentTasksCap {
			return refuse(verb, st.Namespace, "max_concurrent_tasks must be a whole number from 1 to %d (agent_admin.max_concurrent_tasks; the operator can raise it)", st.MaxConcurrentTasksCap)
		}
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
	ints := map[string]int{}
	if concurrency > 0 {
		ints["maxConcurrentTasks"] = concurrency
	}
	content, err := patchProjectMetadata(source, updates, ints)
	if err != nil {
		return refuse(verb, st.Namespace, "project source cannot be edited: %v", err)
	}
	if in.SharedMemory != nil && *in.SharedMemory != p.SharedMemory {
		if content, err = patchSharedMemory([]byte(content), *in.SharedMemory); err != nil {
			return refuse(verb, st.Namespace, "project source cannot be edited: %v", err)
		}
	}
	c := Change{Class: Inert, Ops: []FileOp{{Op: OpReplace, Path: path, Content: content}}, ReadSet: map[string]string{}, Locks: []string{lockProject(pid), path}}
	expect(st, c.ReadSet, path)
	var did []string
	if len(updates) > 0 {
		did = append(did, fmt.Sprintf("updated the name or purpose of project %q", pid))
	}
	// Joining the shared memory is widening; leaving it applies at once
	// (part 24 §24.6 S3).
	if in.SharedMemory != nil && *in.SharedMemory != p.SharedMemory {
		if *in.SharedMemory {
			c.Class = Widening
			c.Sentence = fmt.Sprintf("Your assistant (%s) wants project %q to share memory.", st.Namespace, pid) + sharedMemorySentence(st, pid)
			if len(updates) > 0 || concurrency > 0 {
				c.Sentence += " It also updates the project's other settings."
			}
			return c
		}
		did = append(did, fmt.Sprintf("stopped project %q from sharing memory (its memory is its own again; nothing was moved)", pid))
	}
	// Against the value in force, as set_budget does (§6.9): 0 is no
	// per-project limit, so any number lowers it.
	now := p.MaxConcurrentTasks
	switch {
	case concurrency == 0 || concurrency == now:
	case now > 0 && concurrency > now:
		c.Class = Widening
		c.Sentence = fmt.Sprintf("Your assistant (%s) wants to let project %q run up to %d tasks at once (now %d).", st.Namespace, pid, concurrency, now)
		if len(updates) > 0 {
			c.Sentence += " It also updates the name or purpose."
		}
		return c
	default:
		did = append(did, fmt.Sprintf("set project %q to at most %d tasks at once", pid, concurrency))
	}
	if len(did) == 0 {
		did = append(did, fmt.Sprintf("left project %q as it was", pid))
	}
	c.Sentence = fmt.Sprintf("Your assistant (%s) %s.", st.Namespace, strings.Join(did, ", and "))
	return c
}

// Patch the original mapping instead of re-rendering a template: metadata
// edits must preserve operator settings and comments outside these fields.
func patchProjectMetadata(source []byte, updates map[string]string, ints map[string]int) (string, error) {
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
		if value, ok := updates[key]; ok {
			if err := setScalar(mapping, key, value, "!!str", yaml.DoubleQuotedStyle); err != nil {
				return "", err
			}
		}
	}
	for key, n := range ints {
		if err := setScalar(mapping, key, strconv.Itoa(n), "!!int", 0); err != nil {
			return "", err
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

// setScalar sets key in mapping to one scalar, appending the key when it is
// absent. A value with an anchor is refused: editing it could change other
// settings that alias it.
func setScalar(mapping *yaml.Node, key, value, tag string, style yaml.Style) error {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			node := mapping.Content[i+1]
			if node.Anchor != "" {
				return fmt.Errorf("field %s has an anchor; editing it could change other settings", key)
			}
			node.Kind, node.Tag, node.Value, node.Style, node.Content = yaml.ScalarNode, tag, value, style, nil
			return nil
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, Style: style})
	return nil
}

// patchSharedMemory sets or clears memory.shared_with_namespace in a project
// mapping, keeping every other key (part 24 §24.6 S3). Clearing drops the
// key, and the memory mapping when it is left empty.
func patchSharedMemory(source []byte, shared bool) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return "", err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("expected one project mapping")
	}
	root := doc.Content[0]
	var mem *yaml.Node
	memAt := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "memory" {
			mem, memAt = root.Content[i+1], i
		}
	}
	if shared {
		if mem == nil {
			mem = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "memory"}, mem)
		}
		if err := setScalar(mem, "shared_with_namespace", "true", "!!bool", 0); err != nil {
			return "", err
		}
	} else if mem != nil {
		for i := 0; i+1 < len(mem.Content); i += 2 {
			if mem.Content[i].Value == "shared_with_namespace" {
				mem.Content = append(mem.Content[:i], mem.Content[i+2:]...)
				break
			}
		}
		if len(mem.Content) == 0 {
			root.Content = append(root.Content[:memAt], root.Content[memAt+2:]...)
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
