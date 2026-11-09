package hooks_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// GitHub #687 (LLD 2026-08-20 companion hardening §13.15): uutils coreutils
// mkdir 0.10.0 (Ubuntu 26.04) let more than one concurrent caller create
// the same directory in 13 of 200 measured rounds, so the hooks' mkdir
// claims were not exclusive. The CI runner on Ubuntu 26.04 failed the
// stale-claim and concurrent-send tests below, and so did the 2026.11.3 CE
// export. nonExclusiveMkdir puts the worst case of that race on PATH: a
// mkdir that succeeds when the directory already exists. Every claim must
// still hold.

func nonExclusiveMkdir(t *testing.T) {
	t.Helper()
	mkdirPath, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("no mkdir on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n# Succeeds when the directory exists: uutils mkdir losing a race (#687).\nexec " + mkdirPath + " -p \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "mkdir"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHintsSession_StaleClaimHoldsWithNonExclusiveMkdir_687(t *testing.T) {
	nonExclusiveMkdir(t)
	TestHintsSession_StaleClaimGivenUp_634(t)
}

func TestHintsSession_ConcurrentSendOnceWithNonExclusiveMkdir_687(t *testing.T) {
	nonExclusiveMkdir(t)
	TestHintsSession_ConcurrentTurnsSendOnce_634(t)
}

// A claim is released on every path that took it (review 2ce0 F2): a missed
// release leaves a file a later turn takes for a dead claim, so the session
// would sit claimed. Covers the post-upload release and the beyond-500 cut.
func TestHintsSession_ClaimReleasedAfterUploadAndCut_687(t *testing.T) {
	d := &stubDaemon{hintsStatus: 200, hintsBody: servedCatalogue, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	ids := []string{"commit.review", "design_written.ingest", "test_failed.recall", "design_written.review", "first_prompt.recall", "edits.recall_design"}
	var dirs []string
	for i := 0; i < 90; i++ { // 540 events: the newest sessions are cut
		f := map[string]bool{}
		for _, id := range ids {
			f[id] = false
		}
		dirs = append(dirs, session(t, data, fmt.Sprintf("s%03d", i), f))
	}
	runSession(t, data, srv.URL, "now")
	for _, dir := range dirs {
		if _, err := os.Lstat(filepath.Join(dir, "uploading")); err == nil {
			t.Fatalf("%s: the claim was not released", dir)
		}
	}
}
