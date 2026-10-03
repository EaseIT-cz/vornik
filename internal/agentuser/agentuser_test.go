package agentuser

import "testing"

// D5 (onboarding-hardening-design.md, 2026-10-02): on the reference host
// (uid 1001, userns_mode keep-id, run_as_user "") the published uid-1000 image
// ran as its baked uid and could not read the daemon's 0700 contract mount —
// "contract mount unusable: cannot read /app/input/task.json (running as
// uid:gid 1000:1000)". R3's table is the rule that closes it; every row is
// pinned here, including the fallback chain's keep-id attempt and a bare uid.
func TestResolve_R3Table(t *testing.T) {
	const uid, gid = 1001, 1001
	cases := []struct {
		name      string
		runAsUser string
		userns    string
		rootless  bool
		agnostic  bool
		wantUser  string
		wantWhy   Reason
	}{
		{"configured uid:gid wins over everything", "1000:1000", "keep-id", true, true, "1000:1000", ReasonConfigured},
		{"configured bare uid is passed as given", "1001", "keep-id", true, false, "1001", ReasonConfigured},
		{"configured under rootful still configured", "1000:1000", "", false, true, "1000:1000", ReasonConfigured},
		{"configured under default userns still configured", "1000:1000", "", true, false, "1000:1000", ReasonConfigured},
		{"configured whitespace is trimmed", "  1000:1000 ", "keep-id", true, true, "1000:1000", ReasonConfigured},

		// The incident row: label + keep-id + rootless + empty run_as_user.
		{"label row: configured keep-id", "", "keep-id", true, true, "1001:1001", ReasonLabel},
		{"label row: keep-id case-insensitive", "", "Keep-ID", true, true, "1001:1001", ReasonLabel},
		// The auto-fallback chain's keep-id attempt passes the same value the
		// configured mode does; the resolver cannot tell them apart and must
		// not need to (R1 corrected: applied per attempt).
		{"label row: chain keep-id attempt", "", " keep-id ", true, true, "1001:1001", ReasonLabel},

		{"unlabelled keep-id keeps the baked uid", "", "keep-id", true, false, "", ReasonUnlabelled},

		{"default attempt is not keep-id", "", "", true, true, "", ReasonNotKeepID},
		{"host is not keep-id", "", "host", true, true, "", ReasonNotKeepID},
		{"private is not keep-id", "", "private", true, true, "", ReasonNotKeepID},

		{"rootful keep-id", "", "keep-id", false, true, "", ReasonRootful},
		{"rootful default", "", "", false, false, "", ReasonRootful},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotUser, gotWhy := Resolve(tc.runAsUser, tc.userns, tc.rootless, tc.agnostic, uid, gid)
			if gotUser != tc.wantUser || gotWhy != tc.wantWhy {
				t.Fatalf("Resolve(%q, %q, rootless=%v, agnostic=%v) = (%q, %q), want (%q, %q)",
					tc.runAsUser, tc.userns, tc.rootless, tc.agnostic, gotUser, gotWhy, tc.wantUser, tc.wantWhy)
			}
		})
	}
}

// Only the label row synthesises a --user the operator did not configure.
func TestReason_Synthesised(t *testing.T) {
	for _, r := range []Reason{ReasonConfigured, ReasonUnlabelled, ReasonNotKeepID, ReasonRootful} {
		if r.Synthesised() {
			t.Errorf("%q.Synthesised() = true, want false", r)
		}
	}
	if !ReasonLabel.Synthesised() {
		t.Error("ReasonLabel.Synthesised() = false, want true")
	}
}

// R5: the label is presence-based. The Containerfile stamps "1"; an empty or
// other value still marks the image uid-agnostic, so the doctor and the
// runtime cannot disagree on a value.
func TestUIDAgnostic_PresenceBased(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{"org.opencontainers.image.version": "2026.9.4"}, false},
		{map[string]string{UIDAgnosticLabel: "1"}, true},
		{map[string]string{UIDAgnosticLabel: ""}, true},
		{map[string]string{UIDAgnosticLabel: "0"}, true},
	}
	for _, tc := range cases {
		if got := UIDAgnostic(tc.labels); got != tc.want {
			t.Errorf("UIDAgnostic(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

func TestIsKeepID(t *testing.T) {
	for _, s := range []string{"keep-id", "KEEP-ID", " keep-id", "keep-id:uid=1001,gid=1001"} {
		if !IsKeepID(s) {
			t.Errorf("IsKeepID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "host", "private", "keep", "nomap"} {
		if IsKeepID(s) {
			t.Errorf("IsKeepID(%q) = true, want false", s)
		}
	}
}

// The runtime's fallback chain and the doctor's throwaway default-namespace
// start (R6, review 4ab9 F2) must classify "podman could not set up the user
// namespace" identically, so the classifier lives here, once.
func TestIsUserNSSetupError(t *testing.T) {
	yes := []string{
		`Error: invalid internal status, unable to create a new pause process: cannot set up namespace using "/usr/bin/newuidmap": exit status 1`,
		"newuidmap: write to uid_map failed: Operation not permitted",
		"newgidmap: open of gid_map failed",
		"cannot set up namespace using newuidmap",
	}
	for _, s := range yes {
		if !IsUserNSSetupError([]byte(s)) {
			t.Errorf("IsUserNSSetupError(%q) = false, want true", s)
		}
	}
	no := []string{"", "Error: image not known", "permission denied", "exit status 1"}
	for _, s := range no {
		if IsUserNSSetupError([]byte(s)) {
			t.Errorf("IsUserNSSetupError(%q) = true, want false", s)
		}
	}
}
