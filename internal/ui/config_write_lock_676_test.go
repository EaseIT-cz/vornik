package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #676 (operations design §6.9). Found at review (the #462 design
// review). The admin UI wrote the live config tree holding no configlock, so a
// UI save could interleave with a control-plane apply or another config write.
// While another writer holds the lock, a save must wait and write nothing;
// once released, it writes.
func TestUIConfigWrite676_SaveWaitsForTheLock(t *testing.T) {
	srv, configsDir := newCreateRig(t)
	form := url.Values{"workflowId": {"locked-wf"}, "displayName": {"Locked"}, "stepName": {"go"}, "roleName": {"helper"}}
	target := filepath.Join(configsDir, "workflows", "locked-wf.md")

	unlock := configlock.Lock() // another config writer is mid-write
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/workflows/new", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		srv.WorkflowsCreate(rr, req)
		done <- rr.Code
	}()

	select {
	case code := <-done:
		unlock()
		t.Fatalf("the save finished (%d) while another writer held the config lock", code)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		unlock()
		t.Fatalf("the workflow was written while the config lock was held (stat err=%v)", err)
	}

	unlock()
	select {
	case code := <-done:
		require.Equal(t, http.StatusSeeOther, code)
	case <-time.After(10 * time.Second):
		t.Fatal("the save never finished after the lock was released")
	}
	_, err := os.Stat(target)
	require.NoError(t, err, "the save writes once the lock is free")
}
