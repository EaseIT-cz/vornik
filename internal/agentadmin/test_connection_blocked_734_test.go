package agentadmin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #734 (design part 06, §6.7 item 5). Found at production (a CE
// report): the guard's refusal of a private address answered "unreachable",
// which reads as a network fault and sent the operator to the wrong layer.
// It now has its own closed code.
func TestClassifyConnectionError_734_GuardRefusalIsBlockedPrivateAddress(t *testing.T) {
	refused := &url.Error{Op: "Head", URL: "https://gitlab.corp.example/", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: fmt.Errorf("%w private/loopback/link-local address 10.0.0.5 (host gitlab.corp.example)", dialguard.ErrDialRefused),
	}}
	if code, class := ClassifyConnectionError(refused, 0); code != ConnBlockedPrivateAddress || class != "" {
		t.Fatalf("guard refusal = %q %q, want %q", code, class, ConnBlockedPrivateAddress)
	}
	plain := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if code, _ := ClassifyConnectionError(plain, 0); code != ConnUnreachable {
		t.Fatalf("a plain dial error = %q, want unreachable", code)
	}
	if ConnBlockedPrivateAddress != "blocked_private_address" {
		t.Fatalf("code = %q", ConnBlockedPrivateAddress)
	}
	for _, want := range []string{"blocked_private_address", "agent_admin.private_api_hosts", "https", "never allows loopback or link-local"} {
		if !strings.Contains(TestConnectionRule, want) {
			t.Errorf("TestConnectionRule does not say %q", want)
		}
	}
}
