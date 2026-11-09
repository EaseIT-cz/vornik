package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #681 (operations-api-contract design §6.10, operator decision
// O-676-1): on a daemon that does not hold the config_writer lease, a UI save
// answers 409 and writes nothing.
func TestUIConfigWrite681_RefusedOnANonWriterDaemon(t *testing.T) {
	configlock.SetWriterCheck(func(context.Context) (bool, string) { return false, "lease held by node-b" })
	t.Cleanup(func() { configlock.SetWriterCheck(nil) })
	srv, configsDir := newCreateRig(t)
	form := url.Values{"workflowId": {"fenced-wf"}, "displayName": {"Fenced"}, "stepName": {"go"}, "roleName": {"helper"}}
	req := httptest.NewRequest(http.MethodPost, "/workflows/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.WorkflowsCreate(rr, req)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "not the config writer")
	_, err := os.Stat(filepath.Join(configsDir, "workflows", "fenced-wf.md"))
	require.True(t, os.IsNotExist(err), "a refused save must write nothing")
	release, ok := configlock.TryLock()
	require.True(t, ok, "a refused save must not keep the config lock")
	release()
}
