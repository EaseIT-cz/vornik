package executor

import "strings"

// Writing-style guidance injected into agent system prompts (GitHub #633;
// https://docs.vornik.io §6.1).
//
// WHY THE BINARY AND NOT A PRESET. People stop reading what agents write when
// it is long, vague and hedged, and review turns into rubber-stamping. The
// rule below is true on every deployment and names no customer's house style,
// so it is mechanics (LLD 09 §13.2); as config it would reach new installs
// only. A team with a stricter or different style adds it in rolePrelude.
//
// ADVISORY. It is guidance, not a rule the daemon enforces, so an operator may
// switch it off for a swarm or a role (suppressedGuidanceBlocks).
//
// NOT ON A TOOL-FREE STEP. The strict-adaptive router returns a schema-bound
// pick, not prose for a person.
//
// Bounded at 560 bytes by its own test, so the aggregate budget keeps room.
const writingStyleSystemPromptBlock = `
WRITING FOR PEOPLE: anything a person will read (documents, reports, reviews,
commit messages).
- Lead with the answer. Say each thing once. Short sentences, plain words,
  active voice.
- Give numbers with source and scope, not adjectives.
- Cite exactly: path:line, function, issue number, commit sha or URL; never
  "above" or "the relevant file".
- Hedge only when the uncertainty is real; say what is unknown.
- In code: plain constructs, clear names, no unused abstraction.
- Cut what does not change the reader's understanding; never cut a fact.
`

// composeSystemPromptWithWritingStyle appends the writing-style block after
// the role identity.
func composeSystemPromptWithWritingStyle(prompt string, include bool) string {
	if !include {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return writingStyleSystemPromptBlock
	}
	return prompt + "\n" + writingStyleSystemPromptBlock
}
