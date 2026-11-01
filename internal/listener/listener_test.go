package listener

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A2): the listener a request arrived on is stamped where the
// connection is accepted, so a handler can tell the public listener from the
// operations listener and no client header can change it.

func TestFromContext560_UnstampedIsUnknown(t *testing.T) {
	if got := FromContext(context.Background()); got != Unknown {
		t.Fatalf("FromContext(bare) = %q, want %q", got, Unknown)
	}
	if got := FromContext(With(context.Background(), Operations)); got != Operations {
		t.Fatalf("FromContext(With(Operations)) = %q", got)
	}
}

func TestConnContext560_StampsEachServer(t *testing.T) {
	for _, name := range []Name{Public, Operations, AgentSocket} {
		seen := make(chan Name, 1)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen <- FromContext(r.Context())
		}))
		srv.Config.ConnContext = ConnContext(name)
		srv.Start()
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("X-Vornik-Listener", string(Operations)) // a client cannot choose
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		srv.Close()
		if got := <-seen; got != name {
			t.Errorf("server stamped %q: handler read %q", name, got)
		}
	}
}

func TestConnContext560_KeepsTheBaseContext(t *testing.T) {
	type k struct{}
	base := context.WithValue(context.Background(), k{}, "v")
	ctx := ConnContext(Public)(base, &net.TCPConn{})
	if ctx.Value(k{}) != "v" || FromContext(ctx) != Public {
		t.Fatal("ConnContext must add the stamp to the context it is given")
	}
}
