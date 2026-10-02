package registry

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// DescribeSchemaFailure lists "<path> fails <keyword>" for each leaf cause,
// deliberately dropping the library's message (which can quote the value).
func DescribeSchemaFailure(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return "inputs do not match the input schema"
	}
	var leaves []string
	var walk func(v *jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			kw := v.KeywordLocation
			if i := strings.LastIndex(kw, "/"); i >= 0 {
				kw = kw[i+1:]
			}
			loc := v.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			// A missing field is named at the parent, so name it from the
			// message. The names come from the schema's `required` list,
			// never from the input (Hermes e2e lane, 2026-09-30).
			if kw == "required" {
				if names := MissingRequired(v.Message); len(names) > 0 {
					for _, n := range names {
						leaves = append(leaves, "inputs"+strings.TrimSuffix(loc, "/")+"/"+n+" fails required")
					}
					return
				}
			}
			leaves = append(leaves, "inputs"+loc+" fails "+kw)
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(leaves)
	if len(leaves) > 5 {
		leaves = append(leaves[:5], fmt.Sprintf("and %d more", len(leaves)-5))
	}
	return strings.Join(leaves, "; ")
}

// MissingRequired reads the property names out of jsonschema v5's
// "missing properties: 'a', 'b'" message. It is best effort: anything else
// yields nil and the caller falls back to naming the rule only. The contract
// is the "missing required input names the field" row of
// TestBrokerDelegate_Refusals, which runs the real validator, so a message
// format change in a jsonschema upgrade fails there, not silently.
func MissingRequired(msg string) []string {
	rest, ok := strings.CutPrefix(msg, "missing properties: ")
	if !ok {
		return nil
	}
	var names []string
	for _, q := range strings.Split(rest, ", ") {
		if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
			return nil
		}
		names = append(names, q[1:len(q)-1])
	}
	return names
}
