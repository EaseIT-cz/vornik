package mcpconnect

import "testing"

// GitHub #91 (2026-10-08): one remedy string on every surface that reports
// an oauth server with no grant (auth design §14 item 5; review 5fe0 F1).
func TestConnectCommand_EveryScope(t *testing.T) {
	cases := map[[2]string]string{
		{"", "atlassian"}:                    "vornikctl mcp connect atlassian",
		{"assistant", "atlassian"}:           "vornikctl mcp connect atlassian -p assistant",
		{"hermes--venues", "calendar"}:       "sign in on the approver device (the assistant's request_credential, kind oauth)",
		{"hermes--venues", "calendar-write"}: "sign in on the approver device (the assistant's request_credential, kind oauth)",
	}
	for in, want := range cases {
		if got := ConnectCommand(in[0], in[1]); got != want {
			t.Errorf("ConnectCommand(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
