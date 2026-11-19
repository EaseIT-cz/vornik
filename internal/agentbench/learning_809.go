package agentbench

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), change 1):
// the learning study's arms differ only in three config switches. They are
// read into the learning axis and left out of the config hash, so the four
// arms share one config_sha256 and any other config difference still refuses.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Learning is the arm's learning axis: which self-learning features ran.
// The zero value is the model alone, and adds no pair to the key.
type Learning struct {
	Instincts       bool `json:"instincts,omitempty"`
	BlackboxTrigger bool `json:"blackboxTrigger,omitempty"`
	Auto            bool `json:"auto,omitempty"`
}

// String is the axis value: "" when every switch is off.
func (l Learning) String() string {
	if l == (Learning{}) {
		return ""
	}
	onOff := func(b bool, on string) string {
		if b {
			return on
		}
		return "off"
	}
	return "instincts=" + onOff(l.Instincts, "on") +
		",blackbox=" + onOff(l.BlackboxTrigger, "bench-trigger") +
		",auto=" + onOff(l.Auto, "on")
}

// learningSwitches are the config paths the learning axis carries. Only
// these are removed from the hash; every tuning value stays in it.
var learningSwitches = [][]string{
	{"instinct", "enabled"},
	{"bench", "learning", "trigger", "enabled"},
	{"bench", "learning", "auto"},
}

// ArmConfigIdentity reads a daemon config file: the SHA-256 of its canonical
// form (keys sorted, comments dropped) without the learning switches, and
// the learning axis those switches set.
func ArmConfigIdentity(raw []byte) (string, Learning, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", Learning{}, fmt.Errorf("parse daemon config: %w", err)
	}
	tree, err := stringKeyed(doc)
	if err != nil {
		return "", Learning{}, err
	}
	var l Learning
	for i, path := range learningSwitches {
		on := popBool(tree, path)
		switch i {
		case 0:
			l.Instincts = on
		case 1:
			l.BlackboxTrigger = on
		case 2:
			l.Auto = on
		}
	}
	canon, err := json.Marshal(tree) // encoding/json sorts map keys
	if err != nil {
		return "", Learning{}, err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), l, nil
}

// stringKeyed turns the YAML tree into maps with string keys, which
// encoding/json can marshal.
func stringKeyed(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			n, err := stringKeyed(c)
			if err != nil {
				return nil, err
			}
			t[k] = n
		}
		return t, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, c := range t {
			n, err := stringKeyed(c)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = n
		}
		return out, nil
	case []any:
		for i, c := range t {
			n, err := stringKeyed(c)
			if err != nil {
				return nil, err
			}
			t[i] = n
		}
		return t, nil
	}
	return v, nil
}

// popBool removes the value at path and reports whether it was true.
func popBool(tree any, path []string) bool {
	m, ok := tree.(map[string]any)
	if !ok {
		return false
	}
	if len(path) == 1 {
		v, present := m[path[0]]
		delete(m, path[0])
		b, _ := v.(bool)
		return present && b
	}
	return popBool(m[path[0]], path[1:])
}

// learningPair is the key's learning pair, or nil for an all-off arm.
func (a ArmFields) learningPair() []string {
	if v := a.Learning.String(); v != "" {
		return []string{"learning", v}
	}
	return nil
}
