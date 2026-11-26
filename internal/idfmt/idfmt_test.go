package idfmt

import "testing"

func TestShort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"task ID", "task_20260518075825_d8f66e9c88d3fee9", "T-fee9"},
		{"exec ID", "exec_20260518080037_be319afc75233107", "X-3107"},
		{"execution alias", "execution_20260518_abcd1234", "X-1234"},
		{"tmsg ID", "tmsg_20260518_aabbccdd", "M-ccdd"},
		{"msg alias", "msg_20260518_aabbccdd", "M-ccdd"},
		{"epoch ID", "cep_20260518_aabbccdd", "E-ccdd"},
		{"artifact ID", "art_20260518_aabbccdd", "A-ccdd"},
		{"unknown prefix", "vornik_20260518_aabbccdd", "vornik_20260518_aabbccdd"},
		{"no underscore", "deadbeef", "deadbeef"},
		{"too short", "abc", "abc"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Short(tc.in)
			if got != tc.want {
				t.Errorf("Short(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// GitHub #848 (UI design §23): lease IDs are "lease-" and 32 hex characters
// (newLeaseID) and had no short form.
func TestShort_Lease848(t *testing.T) {
	if got := Short("lease-0123456789abcdef0123456789abfee9"); got != "L-fee9" {
		t.Fatalf("Short(lease) = %q, want L-fee9", got)
	}
	for _, in := range []string{"lease-", "lease", "leasefoo-abcd", "release-abcd1234", "lease-abcdef12", "lease-0123456789abcdef0123456789abfeeZ"} {
		if got := Short(in); got != in {
			t.Errorf("Short(%q) = %q, want it unchanged", in, got)
		}
	}
}

// GitHub #854 (UI design §24): instincts and control-plane proposals get a
// typed short form; an approver device keeps the phone pages' form.
func TestShort_854(t *testing.T) {
	for in, want := range map[string]string{
		"ins_20261125101010_0123456789abfee9": "I-fee9",
		"cpp_20261125101010_0123456789abfee9": "P-fee9",
		"dev_0123456789abcdef":                "dev_…abcdef",
	} {
		if got := Short(in); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
	// Not a device id: unchanged.
	for _, in := range []string{"dev_tools", "dev_abc", "dev_0123456789abcdeZ", "dev_0123456789abcdef00"} {
		if got := Short(in); got != in {
			t.Errorf("Short(%q) = %q, want it unchanged", in, got)
		}
	}
}
