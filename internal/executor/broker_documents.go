package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
)

// Broker design §18.3 and §18.7 F6 (a document input, GREEN at review 7514):
// each document a broker workflow declares is written into the step's own
// workspace as artifacts/in/<property>.<ext>, mode 0444, before the step
// starts. Every step gets a fresh workspace, so every step is staged. The
// value comes from the task's broker_inputs, where delegate (or the
// schedule) left it after checking it; the prompt names the path, never the
// bytes.

// brokerDocumentMode is the staged document's mode: the role reads it and
// cannot rewrite it in place (file_write opens it for writing and is
// refused; file_edit refuses a file without a write bit).
const brokerDocumentMode os.FileMode = 0o444

// stageBrokerDocuments writes wf's declared documents present in the task's
// broker_inputs into workspaceDir/artifacts/in. A workflow without documents
// stages nothing. A value that no longer satisfies its declaration (the bound
// was lowered after the task was created) is refused, never staged; the
// error names the property, not the value.
func stageBrokerDocuments(workspaceDir string, task *persistence.Task, wf *registry.Workflow) error {
	if wf == nil || wf.Broker == nil || task == nil {
		return nil
	}
	docs := wf.Broker.Documents()
	if len(docs) == 0 {
		return nil
	}
	var payload struct {
		Context struct {
			BrokerInputs map[string]any `json:"broker_inputs"`
		} `json:"context"`
	}
	if len(task.Payload) > 0 {
		if err := json.Unmarshal(task.Payload, &payload); err != nil {
			return fmt.Errorf("broker documents: the task payload does not parse")
		}
	}
	inputs := payload.Context.BrokerInputs
	if err := wf.Broker.CheckDocumentValues(inputs); err != nil {
		return fmt.Errorf("broker documents: %w", err)
	}
	inDir := filepath.Join(workspaceDir, "artifacts", "in")
	for _, d := range docs {
		v, present := inputs[d.Property]
		if !present {
			continue
		}
		s, err := stageDocumentValue(d.Property, v)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(inDir, 0o755); err != nil {
			return fmt.Errorf("broker documents: %w", err)
		}
		dst, err := safepath.JoinUnder(inDir, d.FileName())
		if err != nil {
			return fmt.Errorf("broker documents: %s: %w", d.Property, err)
		}
		if err := os.WriteFile(dst, []byte(s), brokerDocumentMode); err != nil {
			return fmt.Errorf("broker documents: %s: %w", d.Property, err)
		}
		// WriteFile's mode is masked by the umask, and an existing file keeps
		// its mode: set it explicitly.
		if err := os.Chmod(dst, brokerDocumentMode); err != nil {
			return fmt.Errorf("broker documents: %s: %w", d.Property, err)
		}
	}
	return nil
}

// stageDocumentValue is a document's text, or an explicit error naming the
// property (never the value) when it is not a string. CheckDocumentValues
// already refuses one; this keeps the staging loop from writing an empty
// file should that check ever change (review 20261003-6fec item 4).
func stageDocumentValue(property string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("broker documents: inputs/%s is not a string of text", property)
	}
	return s, nil
}

// mirrorStagedInputs copies the regular files staged in src into dst (a warm
// container's persistent workspace). Staged inputs are written 0600, as they
// can be operator-private; a read-only source (a broker document) stays
// read-only. warn, when set, receives per-file failures, which skip the file.
func mirrorStagedInputs(src, dst string, warn func(err error, dst string)) error {
	entries, err := os.ReadDir(src)
	if err != nil || len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(src, ent.Name()))
		if rerr != nil {
			continue
		}
		safeName, nerr := safepath.CleanFileName(ent.Name())
		if nerr != nil {
			continue
		}
		target, jerr := safepath.JoinUnder(dst, safeName)
		if jerr != nil {
			continue
		}
		mode := os.FileMode(0o600)
		if info, ierr := ent.Info(); ierr == nil && info.Mode().Perm()&0o222 == 0 {
			mode = brokerDocumentMode
		}
		if werr := os.WriteFile(target, data, mode); werr != nil {
			if warn != nil {
				warn(werr, target)
			}
			continue
		}
		_ = os.Chmod(target, mode)
	}
	return nil
}
