package registry

import (
	"errors"
	"fmt"
	"sort"
)

// Standing grants: broker write-actions design, "Tier 2: standing grants"
// as revised (items 1 and 9) and settled by review 61a5.

// Standing-grant ceilings. The daemon may lower them (broker.standing_grants),
// never raise them.
const (
	StandingMaxDays           = 7
	StandingMaxUses           = 20
	StandingMaxLivePerProject = 10
)

// BrokerStanding declares a proposal eligible for standing grants. Key names
// the arguments a grant pins exactly; it must include every argument the
// args_schema marks x-destination, so a covered write can never add a
// recipient.
type BrokerStanding struct {
	Key     []string `yaml:"key" json:"key"`
	MaxDays int      `yaml:"max_days,omitempty" json:"max_days,omitempty"`
	MaxUses int      `yaml:"max_uses,omitempty" json:"max_uses,omitempty"`
}

// EffectiveMaxDays resolves the default (the ceiling).
func (s *BrokerStanding) EffectiveMaxDays() int {
	if s == nil {
		return 0
	}
	if s.MaxDays <= 0 {
		return StandingMaxDays
	}
	return s.MaxDays
}

// EffectiveMaxUses resolves the default (the ceiling).
func (s *BrokerStanding) EffectiveMaxUses() int {
	if s == nil {
		return 0
	}
	if s.MaxUses <= 0 {
		return StandingMaxUses
	}
	return s.MaxUses
}

// DestinationArgPaths lists the top-level arguments the args_schema marks
// x-destination: true, sorted.
func (p BrokerProposal) DestinationArgPaths() []string {
	props, _ := p.ArgsSchema["properties"].(map[string]any)
	var out []string
	for name, v := range props {
		if node, ok := v.(map[string]any); ok && marked(node, "x-destination") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Destinations is DestinationArgPaths as a set, the shape the key
// canonicalisation takes.
func (p BrokerProposal) Destinations() map[string]bool {
	out := map[string]bool{}
	for _, d := range p.DestinationArgPaths() {
		out[d] = true
	}
	return out
}

func marked(node map[string]any, ext string) bool {
	b, _ := node[ext].(bool)
	return b
}

// validateStanding is the load-time eligibility rule (tier 2 revised, item
// 1): a non-empty key of declared top-level arguments, covering every
// x-destination argument; at least one destination; no argument carrying
// stored content; bounds within the ceilings.
func (p BrokerProposal) validateStanding() error {
	s := p.Standing
	if s == nil {
		return nil
	}
	if s.MaxDays < 0 || s.MaxDays > StandingMaxDays {
		return fmt.Errorf("max_days must be between 1 and %d", StandingMaxDays)
	}
	if s.MaxUses < 0 || s.MaxUses > StandingMaxUses {
		return fmt.Errorf("max_uses must be between 1 and %d", StandingMaxUses)
	}
	if len(s.Key) == 0 {
		return errors.New("key must name at least one argument")
	}
	if path, found := findMarked(p.ArgsSchema, "x-carries-content", ""); found {
		return fmt.Errorf("argument %q is marked x-carries-content: a write that can carry stored content or an attachment cannot be covered by a standing grant", path)
	}
	props, _ := p.ArgsSchema["properties"].(map[string]any)
	// Destinations are top-level, and each is an address the schema bounds.
	for name, v := range props {
		node, _ := v.(map[string]any)
		if node == nil {
			continue
		}
		if path, found := findMarked(node, "x-destination", name); found && path != name {
			return fmt.Errorf("x-destination on %q: a destination must be a top-level argument", path)
		}
		if marked(node, "x-destination") && !addressShaped(node) {
			return fmt.Errorf("x-destination argument %q (maybe a body) must be an address: a string with format: email or a pattern, or a bounded list of them", name)
		}
	}
	dests := p.DestinationArgPaths()
	if len(dests) == 0 {
		return errors.New("the args_schema marks no argument x-destination: true, so a grant could not name who a write goes to")
	}
	seen := map[string]bool{}
	for _, k := range s.Key {
		if seen[k] {
			return fmt.Errorf("key names %q twice", k)
		}
		seen[k] = true
		if _, ok := props[k]; !ok {
			return fmt.Errorf("key names %q, which is not a top-level argument of args_schema", k)
		}
	}
	for _, d := range dests {
		if !seen[d] {
			return fmt.Errorf("key must include every x-destination argument; %q is missing, so a covered write could add a recipient", d)
		}
	}
	return nil
}

// findMarked finds a node under n (n included) marked ext, returning its
// dotted property path.
func findMarked(n map[string]any, ext, at string) (string, bool) {
	if marked(n, ext) {
		return at, true
	}
	if props, ok := n["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if c, ok := props[k].(map[string]any); ok {
				if p, f := findMarked(c, ext, joinSchemaPath(at, k)); f {
					return p, true
				}
			}
		}
	}
	if items, ok := n["items"].(map[string]any); ok {
		return findMarked(items, ext, at)
	}
	return "", false
}

// addressShaped: a string bounded as an address (format: email, or a
// pattern), or an array of such strings.
func addressShaped(node map[string]any) bool {
	switch t, _ := node["type"].(string); t {
	case "string":
		f, _ := node["format"].(string)
		_, pat := node["pattern"].(string)
		return f == "email" || pat
	case "array":
		items, _ := node["items"].(map[string]any)
		return items != nil && addressShaped(items)
	}
	return false
}
