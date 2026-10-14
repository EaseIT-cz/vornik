package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub #365: `vornikctl project delete-now` printed "Scheduled … for
// immediate deletion" whether or not the wipe worked. A failed wipe must exit
// non-zero with the daemon's error; a finished one says Deleted.
func TestProjectDeleteNow_FailedWipeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"DELETE_FAILED","message":"project \"p\" was NOT deleted: db wipe: no such table: memory_embed_queue"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "k")
	err := runProjectDeleteNow(nil, []string{"p"})
	if err == nil || !strings.Contains(err.Error(), "memory_embed_queue") {
		t.Fatalf("err = %v, want the daemon's DELETE_FAILED error", err)
	}
}

func TestPrintDeleteNowOutcome(t *testing.T) {
	for status, want := range map[string]string{
		"deleted":          "Deleted p.",
		"deletion_pending": "still running",
		"archived":         "outcome not reported",
	} {
		var b bytes.Buffer
		printDeleteNowOutcome(&b, &projectArchiveEntry{ProjectID: "p", Status: status})
		if !strings.Contains(b.String(), want) {
			t.Errorf("status %q printed %q, want it to contain %q", status, b.String(), want)
		}
	}
}

// Delta audit 2026-10-14 (lane W8-W, archival design section H). Found at:
// code review; a user observes it through delete-now. A deletion that removed
// the project but left artifact blobs used to exit 1 with "was NOT deleted";
// it must say the project was removed, name the residue and exit 3 (distinct
// from 1 = not deleted, 0 = deleted).
func TestProjectDeleteNow_ResidueExitsThree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_id":"p","status":"deleted_with_errors","warning":"project removed; artifact files may remain: artifact wipe: access denied"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "k")
	var b bytes.Buffer
	err := deleteNowTo(&b, "p")
	if code := ExitCodeOf(err); code != 3 {
		t.Fatalf("exit code = %d (err %v), want 3", code, err)
	}
	if !strings.Contains(b.String(), "Project p removed; artifact files may remain") || !strings.Contains(b.String(), "access denied") {
		t.Errorf("printed %q, want the removal and the residue", b.String())
	}

	if !strings.Contains(b.String(), "not retried") {
		t.Errorf("printed %q, want the artifact step's consequence", b.String())
	}

	// --json prints the body and exits 3 too (review H round 3, finding 8).
	projectDeleteNowJSON = true
	t.Cleanup(func() { projectDeleteNowJSON = false })
	b.Reset()
	err = deleteNowTo(&b, "p")
	if code := ExitCodeOf(err); code != 3 || !strings.Contains(b.String(), `"status": "deleted_with_errors"`) {
		t.Errorf("--json: exit %d, printed %q; want 3 and the body", code, b.String())
	}
}

// 2026.10.6 delta audit (#334), follow-up to section H. Found at: code
// review; a user observes it through delete-now. A residue whose only step
// was the registry reload was printed "artifact files may remain" although
// every artifact was removed. It must name the reload and that the next
// sweeper tick completes the deletion, and still exit 3 (archival design
// section H.5: one exit code, step-specific text).
func TestProjectDeleteNow_ReloadOnlyResidueNamesTheReload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_id":"p","status":"deleted_with_errors","residue_steps":["reload"],` +
			`"warning":"project removed; the registry reload failed, so the archived project stays listed until the sweeper's next tick, which completes the deletion: reload: permission denied"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "k")
	var b bytes.Buffer
	err := deleteNowTo(&b, "p")
	if code := ExitCodeOf(err); code != 3 {
		t.Fatalf("exit code = %d (err %v), want 3", code, err)
	}
	out := b.String()
	if strings.Contains(out, "artifact files may remain") || strings.Contains(out, "not retried") {
		t.Errorf("printed %q: a reload-only residue must not be worded as artifact residue", out)
	}
	for _, want := range []string{"Project p removed; the registry reload failed", "permission denied", "next sweeper tick completes the deletion"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed %q, want it to contain %q", out, want)
		}
	}
}
