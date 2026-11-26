// Package dialguardtest holds the address shapes every outbound address
// guard is tested against, so each guard is checked on the same rows as the
// daemon's one list, dialguard.IsBlockedIP (GitHub #904;
// integrations-hub-design.md, "Change (2026-11-24): #904"). Tests import it;
// production code does not.
package dialguardtest

// Shape is one address and what the one list says about it.
type Shape struct {
	Addr string
	// Blocked: dialguard.IsBlockedIP refuses it.
	Blocked bool
	// Grantable: a private grant may open it (dialguard.IsGrantablePrivate
	// and not a cloud metadata address).
	Grantable bool
}

// Shapes is the table. dialguard's own test checks every row against the
// predicates, so a row cannot drift from the one list. Each range the one
// list refuses has its edges here, with a neighbour outside it, plus the
// metadata addresses and every IPv6 form that embeds an IPv4 address, and
// the ranges the web guards refuse beyond the one list.
var Shapes = []Shape{
	// 0.0.0.0/8 and broadcast.
	{"0.0.0.0", true, false},
	{"0.0.0.1", true, false},
	{"0.255.255.255", true, false},
	{"1.0.0.0", false, false},
	{"255.255.255.255", true, false},
	// RFC 1918.
	{"10.0.0.0", true, true},
	{"10.255.255.255", true, true},
	{"11.0.0.0", false, false},
	{"172.15.255.255", false, false},
	{"172.16.0.0", true, true},
	{"172.31.255.255", true, true},
	{"172.32.0.0", false, false},
	{"192.168.0.0", true, true},
	{"192.168.255.255", true, true},
	{"192.169.0.0", false, false},
	// CGNAT 100.64.0.0/10 (#893) and the Alibaba Cloud metadata address in it.
	{"100.63.255.255", false, false},
	{"100.64.0.0", true, true},
	{"100.100.100.200", true, false},
	{"100.127.255.255", true, true},
	{"100.128.0.0", false, false},
	// Loopback and link-local (169.254.169.254 is the IPv4 metadata address).
	{"127.0.0.1", true, false},
	{"127.255.255.255", true, false},
	{"169.254.0.1", true, false},
	{"169.254.169.254", true, false},
	{"169.254.255.255", true, false},
	// Multicast.
	{"224.0.0.1", true, false},
	{"239.255.255.255", true, false},
	// Ranges only the web guards' table refuses.
	{"192.0.0.1", false, false},
	{"192.0.2.1", false, false},
	{"192.88.99.1", false, false},
	{"198.18.0.1", false, false},
	{"198.19.255.255", false, false},
	{"198.51.100.1", false, false},
	{"203.0.113.1", false, false},
	{"240.0.0.1", false, false},
	{"255.255.255.254", false, false},
	{"8.8.8.8", false, false},
	// IPv6 loopback, unspecified, ULA (with the AWS and GCP IPv6 metadata
	// addresses), link-local, multicast.
	{"::", true, false},
	{"::1", true, false},
	{"fc00::1", true, true},
	{"fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, true},
	{"fd00:ec2::254", true, false},
	{"fd20:ce::254", true, false},
	{"fe80::1", true, false},
	{"febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, false},
	{"ff01::1", true, false},
	{"ff02::1", true, false},
	{"ff0e::1", true, false},
	{"2606:4700::1111", false, false},
	// IPv4-mapped: judged as the IPv4 address.
	{"::ffff:127.0.0.1", true, false},
	{"::ffff:100.100.100.200", true, false},
	{"::ffff:10.0.0.1", true, true},
	{"::ffff:8.8.8.8", false, false},
	// IPv4-compatible ::/96 (deprecated): refused when the IPv4 address is.
	{"::a9fe:a9fe", true, false},
	{"::7f00:1", true, false},
	{"::808:808", false, false},
	// NAT64 64:ff9b::/96: refused when the IPv4 address is.
	{"64:ff9b::a9fe:a9fe", true, false},
	{"64:ff9b::6464:64c8", true, false},
	{"64:ff9b::a00:1", true, false},
	{"64:ff9b::808:808", false, false},
	// Local-use NAT64 64:ff9b:1::/48 is not unwrapped by the one list.
	{"64:ff9b:1::a9fe:a9fe", false, false},
	// 6to4 2002::/16: refused when the IPv4 address is.
	{"2002:a9fe:a9fe::", true, false},
	{"2002:7f00:1::", true, false},
	{"2002:808:808::", false, false},
	// Teredo, documentation, discard, site-local: only the web guards' table.
	{"2001::1", false, false},
	{"2001:db8::1", false, false},
	{"100::1", false, false},
	{"fec0::1", false, false},
}
