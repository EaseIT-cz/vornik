package dialguard

import (
	"errors"
	"net"
	"testing"
)

// GitHub #893, option A (operator decision 2026-11-24; agent-administered
// design part 06 §6.11, "Change (2026-11-24): #893 option A shipped"): CGNAT
// 100.64.0.0/10, where Tailscale assigns tailnet addresses, is private. The
// one list refuses it, and the two grants (AllowPrivate, PrivateHosts) admit
// it, so a listed tailnet host keeps working. 100.100.100.200 is a metadata
// address: refused by the list and by every grant.
func TestIsBlockedIP_RefusesCGNAT_893(t *testing.T) {
	for _, s := range []string{
		"100.64.0.1",
		"100.101.102.103",
		"100.127.255.254",
		"::ffff:100.64.0.1",      // IPv4-mapped
		"::6440:1",               // IPv4-compatible embedding of 100.64.0.1
		"64:ff9b::6440:1",        // NAT64 embedding of 100.64.0.1
		"2002:6440:1::",          // 6to4 embedding of 100.64.0.1
		"100.100.100.200",        // Alibaba Cloud metadata
		"::ffff:100.100.100.200", // the metadata address, mapped
	} {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = false, want true (CGNAT is private since #893)", s)
		}
	}
	for _, s := range []string{"100.63.255.255", "100.128.0.0", "198.51.100.7"} {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = true, want false (outside 100.64.0.0/10)", s)
		}
	}
}

func TestAgentURLGuard_CGNATNeedsTheList_893(t *testing.T) {
	listed := AgentURLGuard("https://nas.tail1234.ts.net/api", names("nas.tail1234.ts.net")).Dialer("nas.tail1234.ts.net")
	for _, addr := range []string{"100.101.1.2:443", "100.64.0.1:443", "100.127.255.254:443", "[::ffff:100.101.1.2]:443"} {
		if err := listed.Control("tcp", addr, nil); err != nil {
			t.Errorf("listed tailnet host refused at %s: %v", addr, err)
		}
	}
	// The grant never reaches a metadata address, loopback, or an embedded form.
	for _, addr := range []string{"100.100.100.200:80", "[::ffff:100.100.100.200]:80", "127.0.0.1:443",
		"[64:ff9b::6440:1]:443", "[::6440:1]:443", "[2002:6440:1::]:443"} {
		if err := listed.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("listed host dialled %s: Control = %v, want ErrDialRefused", addr, err)
		}
	}

	// Not listed: refused at the dial.
	unlisted := AgentURLGuard("https://nas.tail1234.ts.net/api", nil).Dialer("nas.tail1234.ts.net")
	for _, addr := range []string{"100.101.1.2:443", "100.64.0.1:443", "[::ffff:100.101.1.2]:443"} {
		if err := unlisted.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("unlisted tailnet host dialled %s: Control = %v, want ErrDialRefused", addr, err)
		}
	}
	// A plain-http URL gets no grant (§6.7), so it is refused even when listed.
	plain := AgentURLGuard("http://nas.tail1234.ts.net/api", names("nas.tail1234.ts.net")).Dialer("nas.tail1234.ts.net")
	if err := plain.Control("tcp", "100.101.1.2:80", nil); !errors.Is(err, ErrDialRefused) {
		t.Errorf("listed plain-http tailnet host dialled: Control = %v, want ErrDialRefused", err)
	}
}

func TestAllowPrivate_AdmitsCGNAT_893(t *testing.T) {
	d := DialGuard{AllowPrivate: true}.Dialer("models.tailnet")
	for _, addr := range []string{"100.64.0.1:11434", "100.127.255.254:11434"} {
		if err := d.Control("tcp", addr, nil); err != nil {
			t.Errorf("AllowPrivate refused %s: %v (a tailnet model server must keep working)", addr, err)
		}
	}
	for _, addr := range []string{"100.100.100.200:80", "[64:ff9b::6440:1]:80", "[::6440:1]:80", "[2002:6440:1::]:80"} {
		if err := d.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("AllowPrivate dialled %s: %v (metadata or an embedded form)", addr, err)
		}
	}
	// The default guard refuses it.
	if err := (DialGuard{}).Dialer("x").Control("tcp", "100.64.0.1:443", nil); !errors.Is(err, ErrDialRefused) {
		t.Errorf("default guard dialled 100.64.0.1: %v", err)
	}
}

func TestCarriesSharedAddressSpace_893(t *testing.T) {
	for s, want := range map[string]bool{
		"100.64.0.1": true, "::ffff:100.64.0.1": true, "64:ff9b::6440:1": true,
		"::6440:1": true, "2002:6440:1::": true,
		"10.0.0.5": false, "64:ff9b::a00:5": false, "2002:a00:5::": false, "fd00::1": false,
	} {
		if got := CarriesSharedAddressSpace(net.ParseIP(s)); got != want {
			t.Errorf("CarriesSharedAddressSpace(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestIsCloudMetadata_893(t *testing.T) {
	if !IsCloudMetadata(net.ParseIP("100.100.100.200")) || !IsCloudMetadata(net.ParseIP("::ffff:100.100.100.200")) {
		t.Error("IsCloudMetadata(100.100.100.200) = false")
	}
	if IsCloudMetadata(net.ParseIP("100.100.100.201")) {
		t.Error("IsCloudMetadata(100.100.100.201) = true")
	}
}
