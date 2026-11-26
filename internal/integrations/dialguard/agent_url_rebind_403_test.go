package dialguard

import (
	"errors"
	"testing"
)

// GitHub #403 (agent-administered design part 06, §6.10): the grant is by
// name and is judged on the address of each connection, so a listed name
// that resolved to an intranet address when it was listed and resolves to
// this machine or the metadata service at a later dial is refused at that
// dial. One dialer stands for the name across dials: Control runs on every
// resolved candidate, with no decision cached from an earlier dial.
func TestAgentURLGuard_403_EachDialIsJudgedOnItsOwnAddress(t *testing.T) {
	d := AgentURLGuard("https://mcp.lan.example/mcp", names("mcp.lan.example")).Dialer("mcp.lan.example")
	steps := []struct {
		addr  string
		admit bool
	}{
		{"192.0.2.10:443", true},          // at listing
		{"127.0.0.1:443", false},          // rebound to this machine
		{"169.254.169.254:80", false},     // rebound to the metadata service
		{"[::ffff:127.0.0.1]:443", false}, // the mapped form
		{"[fd00:ec2::254]:80", false},     // AWS IPv6 metadata, inside ULA
		{"10.1.2.3:443", true},            // another private host: the name is vouched for, not an address
	}
	for _, s := range steps {
		err := d.Control("tcp", s.addr, nil)
		if s.admit && err != nil {
			t.Errorf("%s refused: %v", s.addr, err)
		}
		if !s.admit && !errors.Is(err, ErrDialRefused) {
			t.Errorf("%s admitted: %v", s.addr, err)
		}
	}
}
