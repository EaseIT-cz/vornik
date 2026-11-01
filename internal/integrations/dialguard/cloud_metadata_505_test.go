package dialguard

import (
	"errors"
	"testing"
)

// GitHub #505, found by the automated security review of a708bd96a: AWS's
// IPv6 instance-metadata address (fd00:ec2::254) and GCP's (fd20:ce::254) are
// in the ULA range fc00::/7, which AllowPrivate admits as private, so a model
// endpoint (or a #502 setup probe) on an IPv6-enabled VM reached the metadata
// service. Alibaba Cloud's 100.100.100.200 (100.64.0.0/10) was not refused by
// any guard. Found at: review, observed at integration (a guarded client
// dialling its configured endpoint). The Control callback is driven directly
// with the resolved addresses, as in the #502 tests.
func TestCloudMetadata505_RefusedInEveryMode(t *testing.T) {
	addrs := []string{
		"169.254.169.254:80",      // AWS, Azure, GCP, OCI IPv4
		"[fd00:ec2::254]:80",      // AWS IMDS over IPv6
		"[fd20:ce::254]:80",       // GCP metadata over IPv6
		"100.100.100.200:80",      // Alibaba Cloud
		"[64:ff9b::6464:64c8]:80", // NAT64 embedding of 100.100.100.200
		"[::ffff:100.100.100.200]:80",
		"[2002:6464:64c8::]:80", // 6to4 embedding of 100.100.100.200
		"[::a9fe:a9fe]:80",      // IPv4-compatible embedding of 169.254.169.254
	}
	guards := map[string]DialGuard{
		"default":              {},
		"AllowPrivate":         {AllowPrivate: true},
		"ModelEndpointGuard()": ModelEndpointGuard(),
	}
	for name, g := range guards {
		d := g.Dialer("models.example")
		for _, a := range addrs {
			if err := d.Control("tcp", a, nil); !errors.Is(err, ErrDialRefused) {
				t.Errorf("%s dialled the metadata address %s: %v", name, a, err)
			}
		}
	}
	// A ULA model server that is not a metadata address stays reachable under
	// AllowPrivate.
	if err := (DialGuard{AllowPrivate: true}).Dialer("models.lan").Control("tcp", "[fd00::5]:11434", nil); err != nil {
		t.Errorf("AllowPrivate refused an ordinary ULA address: %v", err)
	}
}
