package persistence

import "strings"

// TaskMessageAuthorIsAgent reports whether author is an agent author (lead or
// role:<name>): the only messages a thread summary may hide from the lead's
// prompt (GitHub #431). Operator and system rows, and an empty author, are not.
// see LLD § https://docs.vornik.io §16.2
func TaskMessageAuthorIsAgent(author string) bool {
	return author == TaskMessageAuthorLead ||
		(strings.HasPrefix(author, TaskMessageAuthorRolePrefix) && len(author) > len(TaskMessageAuthorRolePrefix))
}
