package persistence

import "testing"

// GitHub #431: only an agent-authored message (lead or role:<name>) may be
// hidden by a thread summary. Design: task-lifecycle-conversational-design.md §16.2.2.
func TestTaskMessageAuthorIsAgent431(t *testing.T) {
	for author, want := range map[string]bool{
		TaskMessageAuthorLead:     true,
		"role:researcher":         true,
		TaskMessageAuthorOperator: false,
		TaskMessageAuthorSystem:   false,
		"role:":                   false,
		"":                        false,
		"Lead":                    false,
		"leader":                  false,
	} {
		if got := TaskMessageAuthorIsAgent(author); got != want {
			t.Errorf("TaskMessageAuthorIsAgent(%q) = %v, want %v", author, got, want)
		}
	}
}
