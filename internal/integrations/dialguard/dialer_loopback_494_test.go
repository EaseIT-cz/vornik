package dialguard

import (
	"errors"
	"testing"
)

// GitHub #494 item 4 (2026-10-21, integrations-hub-design "Amendment
// 2026-10-21 (#494)", change item 2): LoopbackURLGuard trusted the NAME
// localhost. It allow-listed it, and an allow-listed host is dialled with no
// check of the resolved address, so a localhost that resolved anywhere else
// (a broken /etc/hosts or nsswitch.conf) was dialled unchecked. The name must
// now resolve to loopback.
//
// Lower than the found-at level (review; a user would meet it as a fetch of a
// localhost URL) because a test cannot make the name localhost resolve
// elsewhere without editing /etc/hosts. The Control callback is the exact
// point where the resolved address is checked, so it is driven directly with
// the addresses a resolver could return.
func TestLoopbackHosts_LocalhostMustResolveToLoopback_Issue494(t *testing.T) {
	d := LoopbackURLGuard("http://localhost:8788/caps", nil).Dialer("localhost")
	if d.Control == nil {
		t.Fatal("the dialer for the name localhost has no resolved-address check")
	}
	for _, addr := range []string{"192.0.2.1:8788", "10.0.0.5:8788", "169.254.169.254:80", "[2001:db8::1]:8788"} {
		err := d.Control("tcp", addr, nil)
		if !errors.Is(err, ErrDialRefused) {
			t.Errorf("localhost resolved to %s: Control = %v, want ErrDialRefused", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:8788", "127.0.0.53:8788", "[::1]:8788", "[::ffff:127.0.0.1]:8788"} {
		if err := d.Control("tcp", addr, nil); err != nil {
			t.Errorf("localhost resolved to loopback %s: Control = %v, want nil", addr, err)
		}
	}

	// An IP literal URL: the dial address is the literal, and it is loopback.
	lit := LoopbackURLGuard("http://127.0.0.1:8788", nil).Dialer("127.0.0.1")
	if lit.Control == nil {
		t.Fatal("the loopback literal is dialled without the loopback check")
	}
	if err := lit.Control("tcp", "127.0.0.1:8788", nil); err != nil {
		t.Errorf("127.0.0.1 literal: Control = %v, want nil", err)
	}

	// A name not in the URL is not loopback-listed: the ordinary guard
	// applies (loopback refused).
	other := LoopbackURLGuard("http://localhost:8788", nil).Dialer("broker.lan")
	if err := other.Control("tcp", "127.0.0.1:8788", nil); !errors.Is(err, ErrDialRefused) {
		t.Errorf("unlisted name dialling loopback: Control = %v, want ErrDialRefused", err)
	}

	// An operator AllowedHosts entry still wins: the operator vouches for it.
	vouched := LoopbackURLGuard("http://localhost:8788", []string{"localhost"}).Dialer("localhost")
	if vouched.Control != nil {
		t.Error("allow-listed localhost: the dialer checks the resolved address, want the operator's bypass")
	}
}
