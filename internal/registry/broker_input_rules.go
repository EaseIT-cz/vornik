package registry

import (
	"fmt"
	"sort"
	"strings"
)

// BrokerInputRule is one rule a broker workflow's input schema (or a broker
// task's inputs) must follow. The validator's refusals carry Text verbatim,
// and describe_installation publishes the list, so an agent can read the
// rules before it writes a schema instead of learning them from refusals
// (agent-administered design §18.2). One source: a test checks both
// directions.
type BrokerInputRule struct {
	ID   string `json:"id"`
	Text string `json:"rule"`
}

var (
	ruleKeywords = BrokerInputRule{"keywords", "no " + strings.Join(brokerRefusedKeywords, ", ") +
		": bounds are proved only through plain properties and items"}
	ruleType   = BrokerInputRule{"type", "every schema node has one type (object, array, string, integer, number, boolean or null) or an enum or const"}
	ruleObject = BrokerInputRule{"object", "every object declares additionalProperties: false; property names contain no dots or brackets"}
	ruleArray  = BrokerInputRule{"array", "every array declares maxItems and one items schema"}
	ruleString = BrokerInputRule{"string", "every string is bounded: an enum, an allowed format (" +
		strings.Join(allowedFormatNames(), ", ") + "), a pattern with maxLength, or x-untrusted with maxLength"}
	ruleUntrusted = BrokerInputRule{"untrusted", fmt.Sprintf("an x-untrusted string declares maxLength from 1 to %d", BrokerUntrustedStringMax)}
	rulePattern   = BrokerInputRule{"pattern", fmt.Sprintf("a pattern-constrained string also declares maxLength from 1 to %d", BrokerUntrustedStringMax)}
	ruleBudget    = BrokerInputRule{"budget", fmt.Sprintf("the x-untrusted budget (each maxLength times every enclosing maxItems, summed) is at most %d characters", BrokerUntrustedBudget)}
	ruleTyped     = BrokerInputRule{"typed_inputs_only", "a broker workflow takes typed inputs only: no free-text prompt and no attachments. " +
		"A declared x-untrusted-document is the one way to pass a text document: bounded, text only, staged as a read-only file and never inlined into the prompt or returned; " +
		"anything else larger than an input allows must come from an approved API or MCP connection"}
	// ruleDocument is broker design §18.7 F2 (review 7514).
	ruleDocument = BrokerInputRule{"document", fmt.Sprintf("a top-level x-untrusted-document string carries one text document: "+
		"text/plain, text/markdown or text/x-diff, max_bytes from 1 to %s, at most two per workflow and %s bytes together, "+
		"named by a property matching ^[a-z][a-z0-9_]{0,31}$", groupThousands(BrokerDocumentMaxBytes), groupThousands(BrokerDocumentsTotalMaxBytes))}
)

// BrokerInputRules returns the published rules, in a stable order.
func BrokerInputRules() []BrokerInputRule {
	return []BrokerInputRule{ruleKeywords, ruleType, ruleObject, ruleArray, ruleString, ruleUntrusted, rulePattern, ruleBudget, ruleDocument, ruleTyped}
}

func allowedFormatNames() []string {
	out := make([]string, 0, len(brokerAllowedFormats))
	for f := range brokerAllowedFormats {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// ruleError is a refusal at a schema path that names its rule, with any
// detail after it.
func ruleError(at string, r BrokerInputRule, detail string) error {
	if detail != "" {
		return fmt.Errorf("broker.%s: %s (%s)", at, r.Text, detail)
	}
	return fmt.Errorf("broker.%s: %s", at, r.Text)
}

// groupThousands writes n with comma thousands separators (262,144).
func groupThousands(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
