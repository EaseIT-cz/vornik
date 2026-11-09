package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #670 (task-lifecycle-conversational design §18a): the summarize check
// listed the task's first 500 messages and refused any named id after them, so
// the newer part of a long thread could not be summarized.
func TestSummarize_670_IdAfterThe500th(t *testing.T) {
	f := newSum431(t)
	ctx := context.Background()
	base := time.Now().UTC()
	var late string
	for i := 0; i < 505; i++ {
		m := &persistence.TaskMessage{TaskID: sum431Task, AuthorKind: persistence.TaskMessageAuthorLead,
			MessageKind: persistence.TaskMessageKindMessage, Content: fmt.Sprintf("lead note %d", i),
			CreatedAt: base.Add(time.Duration(i+1) * time.Millisecond)}
		require.NoError(t, f.msgs.Insert(ctx, m))
		late = m.ID
	}
	rec := f.post(t, f.keys["task"], []string{late}, "the late part of a long thread")
	require.Equal(t, http.StatusOK, rec.Code, "an id after the 500th message must be accepted: %s", rec.Body.String())
}
