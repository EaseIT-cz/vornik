package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/workflowapply"
)

// GitHub #462: the apply and rollback refusals (another config write holds
// the lock; this daemon is not the config writer) answer 409 with their own
// codes, not 500. The errors are workflowapply's own values, wrapped as the
// applier wraps them, so the match text cannot drift from the messages.
func TestWorkflowProposals462_FenceAndBusyMapTo409(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"busy", workflowapply.ErrConfigBusy, "APPLY_IN_PROGRESS"},
		{"fenced", fmt.Errorf("%w (%s)", workflowapply.ErrWriterFenced, "superseded by a newer leader epoch"), "WRITER_FENCED"},
	}
	for _, tc := range cases {
		for _, action := range []string{"apply", "rollback"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				s := NewServer(
					WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
					WithWorkflowProposals(&stubProposalRepo{}),
					WithWorkflowApplier(&stubWorkflowApplier{err: tc.err}),
					WithWorkflowRollbacker(&stubWorkflowRollbacker{err: tc.err}),
				)
				rec := httptest.NewRecorder()
				req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
					"/api/v1/admin/workflow-proposals/wpr-1/"+action, nil), "sk-admin")
				s.AdminWorkflowProposalsItem(rec, req)
				if rec.Code != http.StatusConflict {
					t.Fatalf("want 409, got %d (body=%q)", rec.Code, rec.Body.String())
				}
				if !bytes.Contains(rec.Body.Bytes(), []byte(tc.code)) {
					t.Errorf("error code %q missing from body %q", tc.code, rec.Body.String())
				}
			})
		}
	}
}
