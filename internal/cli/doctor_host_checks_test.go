package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Process-spawn law, S2 (https://docs.vornik.io).
// Incident: POST /api/v1/doctor ran podman, skopeo and systemctl on the daemon
// host, and ?fix=true ran git. The daemon now reports a "host_checks" pointer
// instead, and vornikctl runs those checks on the host and merges them in its
// place, so the operator still sees one report.

const daemonReportWithPointer = `{"timestamp":"t","summary":"s","daemon_revision":"0123456789ab","checks":[` +
	`{"name":"stale_leases","status":"OK","message":"none","a_field_added_later":"x"},` +
	`{"name":"host_checks","status":"SKIPPED","message":"host checks run on the host: vornikctl doctor"},` +
	`{"name":"orphan_worktrees","status":"OK","message":"removed 1","fixed":1,"removed":["proj/task_x"]}]}`

func withDoctorServer(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

// fakeHostChecks replaces the local host-check run and records its input.
func fakeHostChecks(t *testing.T) *string {
	t.Helper()
	var gotRev = new(string)
	*gotRev = "<not called>"
	orig := doctorHostChecks
	doctorHostChecks = func(_ context.Context, daemonRevision string) []doctorCheck {
		*gotRev = daemonRevision
		return []doctorCheck{
			{Name: "podman_config", Status: "OK", Message: "podman OK"},
			{Name: "image_freshness", Status: "OK", Message: "images match"},
		}
	}
	t.Cleanup(func() { doctorHostChecks = orig })
	return gotRev
}

func resetDoctorFlags(t *testing.T, jsonOut, fix bool) {
	t.Helper()
	doctorJSON, doctorFix, doctorOffline = jsonOut, fix, false
	t.Cleanup(func() { doctorJSON, doctorFix, doctorOffline = false, false, false })
}

func TestDoctor_RunsHostChecksLocallyInPlaceOfThePointer(t *testing.T) {
	withDoctorServer(t, daemonReportWithPointer)
	gotRev := fakeHostChecks(t)
	resetDoctorFlags(t, false, false)

	out, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if *gotRev != "0123456789ab" {
		t.Errorf("host checks got daemon revision %q, want the daemon's from its report", *gotRev)
	}
	if !strings.Contains(out, "podman_config") || !strings.Contains(out, "image_freshness") {
		t.Errorf("the locally-run host checks are missing:\n%s", out)
	}
	if strings.Contains(out, "host_checks") {
		t.Errorf("the pointer must be replaced, not printed:\n%s", out)
	}
}

// A body with no `checks` key, or a null one, merges to an empty list rather
// than failing the whole --json output: unmarshalling a missing RawMessage is
// "unexpected end of JSON input" (S2 code review, review-20260926-f198 #8).
func TestMergeHostChecksJSON_ToleratesMissingOrNullChecks(t *testing.T) {
	for _, body := range []string{`{"summary":"x"}`, `{"checks":null,"summary":"x"}`} {
		out, err := mergeHostChecksJSON(context.Background(), []byte(body), "")
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		var got struct {
			Checks []json.RawMessage `json:"checks"`
		}
		if err := json.Unmarshal(out, &got); err != nil || got.Checks == nil {
			t.Fatalf("%s: want an empty checks list, got %s (%v)", body, out, err)
		}
	}
}

func TestDoctorJSON_MergesHostChecksAndKeepsDaemonFields(t *testing.T) {
	withDoctorServer(t, daemonReportWithPointer)
	fakeHostChecks(t)
	resetDoctorFlags(t, true, false)

	out, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		DaemonRevision string           `json:"daemon_revision"`
		Summary        string           `json:"summary"`
		Checks         []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	var names []string
	for _, c := range got.Checks {
		names = append(names, c["name"].(string))
	}
	want := "stale_leases,podman_config,image_freshness,orphan_worktrees"
	if strings.Join(names, ",") != want {
		t.Errorf("checks = %v, want %s (host checks in the pointer's place)", names, want)
	}
	if got.Checks[0]["a_field_added_later"] != "x" {
		t.Error("a daemon field the CLI does not declare was dropped")
	}
	if got.DaemonRevision != "0123456789ab" {
		t.Errorf("daemon_revision dropped: %q", got.DaemonRevision)
	}
	if got.Summary != "All checks passed" {
		t.Errorf("summary = %q, want it recomputed over the merged checks", got.Summary)
	}
}

// A daemon from before S2 still runs the host checks itself and sends no
// pointer; vornikctl must not run them a second time.
func TestDoctor_OlderDaemonWithoutThePointerRunsNothingLocally(t *testing.T) {
	withDoctorServer(t, `{"timestamp":"t","summary":"All checks passed","checks":[`+
		`{"name":"podman_config","status":"OK","message":"podman OK"}]}`)
	gotRev := fakeHostChecks(t)
	resetDoctorFlags(t, false, false)
	if _, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if *gotRev != "<not called>" {
		t.Error("host checks ran locally although the daemon already ran them")
	}
}

// --fix: the daemon removes orphan worktree directories and names them; the git
// side of each (worktree prune, branch -D) is cleaned on the host.
func TestDoctorFix_CleansGitForTheWorktreesTheDaemonRemoved(t *testing.T) {
	withDoctorServer(t, daemonReportWithPointer)
	fakeHostChecks(t)
	resetDoctorFlags(t, false, true)
	var got []string
	orig := doctorWorktreeGitCleanup
	doctorWorktreeGitCleanup = func(removed []string) []string {
		got = append(got, removed...)
		return nil
	}
	t.Cleanup(func() { doctorWorktreeGitCleanup = orig })

	if _, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "proj/task_x" {
		t.Errorf("git cleanup ran for %v, want exactly the removed worktree proj/task_x", got)
	}
}
