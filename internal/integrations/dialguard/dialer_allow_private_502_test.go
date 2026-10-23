package dialguard

import (
	"errors"
	"testing"
)

// GitHub #502 (18-security-and-tenancy.md, "Amendment 2026-10-22 (#502)",
// S3): the setup probes dialled the caller's endpoint with an unguarded
// client, so a probe could reach the cloud metadata service. They now dial
// through DialGuard{AllowPrivate: true}, which admits loopback and private
// addresses (a local Ollama, a LAN model server) and refuses every other
// address isBlockedIP refuses. The Control callback is where the resolved
// address is checked, so it is driven directly with the addresses a
// resolver could return (a ULA route need not exist on the test host).
func TestAllowPrivate_AdmitsLoopbackAndPrivateOnly_Issue502(t *testing.T) {
	d := DialGuard{AllowPrivate: true}.Dialer("models.lan")
	if d.Control == nil {
		t.Fatal("AllowPrivate dialer has no resolved-address check: the guard is off")
	}
	for _, addr := range []string{"127.0.0.1:11434", "[::1]:11434", "192.168.1.20:11434", "10.0.0.5:443", "172.16.0.9:8080", "[fd00::1]:11434"} {
		if err := d.Control("tcp", addr, nil); err != nil {
			t.Errorf("AllowPrivate refused %s: %v", addr, err)
		}
	}
	for _, addr := range []string{
		"169.254.169.254:80",      // cloud metadata
		"[fe80::1]:80",            // IPv6 link-local
		"0.0.0.0:80",              // this host, by another name
		"0.1.2.3:80",              // 0.0.0.0/8
		"255.255.255.255:80",      // limited broadcast
		"224.0.0.1:80",            // multicast
		"[ff02::1]:80",            // IPv6 multicast
		"[64:ff9b::a9fe:a9fe]:80", // NAT64 embedding of the metadata address
		"[::a9fe:a9fe]:80",        // IPv4-compatible embedding
		"[2002:a9fe:a9fe::]:80",   // 6to4 embedding
		"[64:ff9b::c0a8:101]:80",  // NAT64 embedding of a LAN address: over-refused
	} {
		if err := d.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("AllowPrivate dialled %s: Control = %v, want ErrDialRefused", addr, err)
		}
	}

	// A public address is dialled under either setting: isBlockedIP never
	// listed it, so AllowPrivate is not what admits it.
	if err := d.Control("tcp", "198.51.100.7:443", nil); err != nil {
		t.Errorf("AllowPrivate refused a public address: %v", err)
	}

	// The default guard is unchanged: private and loopback stay refused.
	def := DialGuard{}.Dialer("models.lan")
	for _, addr := range []string{"192.168.1.20:11434", "127.0.0.1:11434"} {
		if err := def.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("default guard dialled %s: Control = %v, want ErrDialRefused", addr, err)
		}
	}
}
