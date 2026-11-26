package dialguard

import (
	"errors"
	"testing"
)

// GitHub #168: the widening reaches LoopbackURLGuard. An upper-case LOCALHOST
// URL is now a LoopbackHosts entry (before: refused outright as loopback by
// the IP check), so it must get the #494 rule: dialled only when it resolves
// to loopback, never elsewhere.
func TestLoopbackURLGuard_UpperCaseLocalhostResolvesOnlyToLoopback_168(t *testing.T) {
	d := LoopbackURLGuard("http://LOCALHOST:8788/caps", nil).Dialer("LOCALHOST")
	if d.Control == nil {
		t.Fatal("LOCALHOST is dialled with no resolved-address check")
	}
	if err := d.Control("tcp", "192.0.2.1:8788", nil); !errors.Is(err, ErrDialRefused) {
		t.Errorf("LOCALHOST resolved to 192.0.2.1: Control = %v, want ErrDialRefused", err)
	}
	if err := d.Control("tcp", "127.0.0.1:8788", nil); err != nil {
		t.Errorf("LOCALHOST resolved to 127.0.0.1: Control = %v, want nil", err)
	}
}

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)"):
// IsLoopbackHost is the daemon's one rule for "this host names the local
// machine". Fourteen copies disagreed on case, on 127.0.0.0/8 and on input
// shape; this table pins every form the amendment names.
func TestIsLoopbackHost_168(t *testing.T) {
	for host, want := range map[string]bool{
		// The name localhost, any case, bare or with a port.
		"localhost": true, "LOCALHOST": true, "LocalHost": true,
		"localhost:8080": true, "LOCALHOST:8080": true,
		// 127.0.0.0/8, bare or with a port.
		"127.0.0.1": true, "127.0.0.2": true, "127.255.255.254": true,
		"127.0.0.1:80": true, "127.5.5.5:8080": true,
		// ::1, bare, bracketed, with a port.
		"::1": true, "[::1]": true, "[::1]:8080": true, "0:0:0:0:0:0:0:1": true,
		// IPv4-mapped loopback.
		"::ffff:127.0.0.1": true, "[::ffff:127.0.0.1]": true, "[::ffff:127.0.0.1]:8080": true,

		// A trailing dot: resolved by a dial, so refused outbound (#494).
		"localhost.": false, "localhost.:8080": false, "127.0.0.1.": false,
		// Names that only start or end like localhost.
		"localhost.evil.com": false, "evil.localhost": false, "foo.localhost": false,
		"localhostx": false, "xlocalhost": false,
		// Names that embed a loopback literal.
		"127.0.0.1.nip.io": false, "127.0.0.1.evil.com": false,
		// Listen-everywhere addresses are not loopback.
		"0.0.0.0": false, "0.0.0.0:8080": false, "::": false, "[::]": false, "[::]:8080": false, ":8080": false,
		// Short forms, zones, whitespace, empty.
		"127.1": false, "::1%lo": false, "[::1%lo]:80": false, " localhost": false, "localhost ": false, "": false, "[]": false,
		// Other hosts.
		"192.168.1.50": false, "10.0.0.1:8080": false, "example.com": false, "[fe80::1]:80": false,
		"::ffff:10.0.0.1": false,
		// Brackets enclose an IP literal only (review 4dca item 3).
		"[localhost]": false, "[localhost]:80": false, "[LOCALHOST]:80": false, "[127.0.0.1]": false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// GitHub #168, review 4dca item 1: a caller that dials with a client which
// does not check the resolved address (agentbridge, telemetryclient) gets no
// new name spelling. It accepts a loopback IP literal (dialled as itself) or
// exactly "localhost", as before #168; LOCALHOST is refused there.
func TestIsLoopbackHostForPlainDial_168(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.0.0.2": true, "::1": true, "::ffff:127.0.0.1": true,
		"LOCALHOST": false, "LocalHost": false, "localhost.": false, "foo.localhost": false,
		"localhost:8080": false, "[::1]": false, "[localhost]": false, "0.0.0.0": false, "": false,
		"example.com": false, "10.0.0.1": false,
	} {
		if got := IsLoopbackHostForPlainDial(host); got != want {
			t.Errorf("IsLoopbackHostForPlainDial(%q) = %v, want %v", host, got, want)
		}
	}
}
