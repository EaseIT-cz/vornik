package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence/mocks"
)

// audit 2026-10-12 (finding H, review-20261012-2378 L1): REST POST /tasks
// accepts inputArtifacts[].path too; without a repo scope its document would
// pile up live versions exactly as the companion delegate's did. Refused the
// same way (memory-rollback-supersession design A.2, amended 2026-10-12).
func TestCoreAPI_CreateTask_DocumentPathNeedsARepoScope(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		refused bool
	}{
		{"path without scope", `{"taskType":"x","inputArtifacts":[{"name":"a.md","content":"aGk=","path":"docs/a.md"}]}`, true},
		{"path with scope", `{"taskType":"x","context":{"repo_scope":"github.com/acme/w"},"inputArtifacts":[{"name":"a.md","content":"aGk=","path":"docs/a.md"}]}`, false},
		{"no path", `{"taskType":"x","inputArtifacts":[{"name":"a.md","content":"aGk="}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			taskRepo := &mocks.MockTaskRepository{}
			server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))
			server.inputArtifactStore = &fakeInputArtifactStore{}
			req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(c.body))
			rec := httptest.NewRecorder()
			server.CreateTask(rec, req)
			if c.refused {
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "repo_scope")
				assert.Equal(t, 0, taskRepo.CallCount.Create)
				return
			}
			assert.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}
