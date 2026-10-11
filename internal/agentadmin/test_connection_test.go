package agentadmin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"
)

// GitHub #101 (2026-10-08): an assistant could learn whether a stored
// credential works only by running a workflow. test_connection answers with
// one closed code (design §6.2); the classifier reads typed errors and the
// status only, never a message a server could steer.
func TestClassifyConnectionError(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	cases := []struct {
		name   string
		err    error
		status int
		code   ConnectionCode
		class  string
	}{
		{"answered 200", nil, 200, ConnOK, "2xx"},
		{"answered 404 on the base", nil, 404, ConnOK, "4xx"},
		{"answered 302 unfollowed", nil, 302, ConnOK, "3xx"},
		{"401", nil, 401, ConnAuthRejected, "4xx"},
		{"403", nil, 403, ConnAuthRejected, "4xx"},
		{"429 is neither", nil, 429, ConnUnknown, "4xx"},
		{"500", nil, 500, ConnUnknown, "5xx"},
		{"MCP call error 401", errors.New("initialize failed"), 401, ConnAuthRejected, "4xx"},
		{"MCP call error 404", errors.New("initialize failed"), 404, ConnUnknown, "4xx"},
		{"dial refused", fmt.Errorf("wrapped: %w", opErr), 0, ConnUnreachable, ""},
		{"dns", &net.DNSError{Err: "no such host", Name: "x.invalid"}, 0, ConnUnreachable, ""},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), 0, ConnUnreachable, ""},
		{"unknown authority", fmt.Errorf("x: %w", x509.UnknownAuthorityError{}), 0, ConnTLSError, ""},
		{"hostname", fmt.Errorf("x: %w", x509.HostnameError{Host: "h"}), 0, ConnTLSError, ""},
		{"verification", fmt.Errorf("x: %w", &tls.CertificateVerificationError{Err: errors.New("bad")}), 0, ConnTLSError, ""},
		{"plain http behind https", fmt.Errorf("x: %w", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), 0, ConnTLSError, ""},
		{"tls inside an op error", &net.OpError{Op: "remote error", Err: x509.UnknownAuthorityError{}}, 0, ConnTLSError, ""},
		{"anything else", errors.New("protocol error"), 0, ConnUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, class := ClassifyConnectionError(tc.err, tc.status)
			if code != tc.code || class != tc.class {
				t.Fatalf("got (%s, %q), want (%s, %q)", code, class, tc.code, tc.class)
			}
		})
	}
}

// GitHub #101 (2026-10-08): the answer is exactly {code, http_status_class,
// checked_at}; nothing an upstream sent can ride along.
func TestConnectionResult_HasExactlyThreeKeys(t *testing.T) {
	raw, err := json.Marshal(ConnectionResult{Code: ConnOK, HTTPStatusClass: "", CheckedAt: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if fmt.Sprint(keys) != "[checked_at code http_status_class]" {
		t.Fatalf("keys = %v", keys)
	}
}

// GitHub #101 (2026-10-08): the project follows ProjectRefRule (slug or the
// caller's own full id), the name is an integration name; anything else is
// refused before any lookup.
func TestParseTestConnectionInput(t *testing.T) {
	for raw, want := range map[string]TestConnectionInput{
		`{"project":"finance","name":"fio"}`:         {Project: "finance", Name: "fio"},
		`{"project":"hermes--finance","name":"fio"}`: {Project: "finance", Name: "fio"},
	} {
		got, err := ParseTestConnectionInput("hermes", json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		`{"project":"finance"}`,
		`{"project":"other--finance","name":"fio"}`,
		`{"project":"finance","name":"Bad Name"}`,
		`{"project":"finance","name":"fio-write"}`,
		`{"project":"finance","name":"fio","url":"http://x"}`,
	} {
		if _, err := ParseTestConnectionInput("hermes", json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{0: "", 99: "", 100: "1xx", 204: "2xx", 399: "3xx", 404: "4xx", 503: "5xx", 600: ""} {
		if got := StatusClass(status); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", status, got, want)
		}
	}
}
