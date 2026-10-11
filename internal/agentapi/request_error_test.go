package agentapi

import (
	"context"
	"errors"
	"net"
	"testing"

	"vornik.io/vornik/internal/apigateway"
)

// GitHub #101 (2026-10-08): test_connection must tell a TLS failure from an
// unreachable host, so a failed request keeps its cause for errors.As. The
// role-facing text a query_api caller sees stays byte-identical (review 00b7
// finding 7).
func TestClient_FailedRequestKeepsCauseAndText(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()
	c, _, _ := newClient(t, dead, false, "GET", "HEAD")
	_, err = c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: "GET", Path: "x"})
	if err == nil {
		t.Fatal("a call to a closed port succeeded")
	}
	if got, want := err.Error(), apigateway.ErrGatewayRequest.Error()+": the request did not complete"; got != want {
		t.Fatalf("role-facing text changed: %q, want %q", got, want)
	}
	if !errors.Is(err, apigateway.ErrGatewayRequest) {
		t.Fatal("the sentinel is lost")
	}
	var op *net.OpError
	if !errors.As(err, &op) {
		t.Fatalf("the cause is not reachable: %#v", err)
	}
}
