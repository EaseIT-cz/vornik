package cli

import (
	"testing"
)

// GitHub #867: vornikctl doctor exited 1 on a SKIPPED check, though its
// summary counts SKIPPED as no issue (doctor skipped-vs-OK design, amendment
// 2026-11-24). Found at: review (#727 re-run, finding R065; a user sees it as
// the CLI's exit code). Regression test at: unit, through runDoctor against
// a fake daemon, which is where the exit code is decided. Lower because: the
// process exit is main's ExitCodeOf(err), tested in leader_lock_test.go.

func doctorReportWith(statuses ...string) string {
	body := `{"timestamp":"t","summary":"s","daemon_revision":"0123456789ab","checks":[`
	for i, s := range statuses {
		if i > 0 {
			body += ","
		}
		body += `{"name":"c` + string(rune('a'+i)) + `","status":"` + s + `","message":"m"}`
	}
	return body + `]}`
}

func TestDoctorExitCode867_OKAndSkippedExitZero(t *testing.T) {
	withDoctorServer(t, doctorReportWith("OK", "SKIPPED", "OK", "skipped"))
	fakeHostChecks(t)
	resetDoctorFlags(t, false, false)
	if _, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) }); err != nil {
		t.Fatalf("doctor with only OK and SKIPPED checks = %v (exit %d), want exit 0", err, ExitCodeOf(err))
	}
}

func TestDoctorExitCode867_AnIssueExitsOne(t *testing.T) {
	for _, status := range []string{"WARNING", "ERROR", "SOMETHING_NEW"} {
		t.Run(status, func(t *testing.T) {
			withDoctorServer(t, doctorReportWith("OK", "SKIPPED", status))
			fakeHostChecks(t)
			resetDoctorFlags(t, false, false)
			_, err := captureStdoutFunc(t, func() error { return runDoctor(doctorCmd, nil) })
			if ExitCodeOf(err) != 1 {
				t.Fatalf("doctor with a %s check: exit %d (err %v), want 1", status, ExitCodeOf(err), err)
			}
			if err.Error() != "" {
				t.Errorf("the exit error prints %q; the report already said it, want no error line", err.Error())
			}
		})
	}
}
