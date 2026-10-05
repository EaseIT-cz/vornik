package agentapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/apigateway"
)

func TestClient_AuditRedirectBoundary(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, target := range []string{"other-port", "outside-base", "same-base", "scheme-change"} {
				t.Run(method, func(t *testing.T) {
					t.Run(target, func(t *testing.T) {
						var calls atomic.Int32
						var leaked atomic.Bool
						destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							leaked.Store(r.Header.Get("Authorization") != "")
							w.WriteHeader(http.StatusOK)
						}))
						defer destination.Close()
						source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path != "/v1/start" {
								calls.Add(1)
								leaked.Store(r.Header.Get("Authorization") != "")
								w.WriteHeader(http.StatusOK)
								return
							}
							if r.Method != method || r.Header.Get("Authorization") != "Bearer "+credCanary {
								t.Error("initial approved request lost its method or credential")
							}
							location := destination.URL + "/stolen"
							if target == "outside-base" {
								location = "/admin"
							}
							if target == "same-base" {
								location = "/v1/other"
							}
							if target == "scheme-change" {
								location = "https://" + r.Host + "/v1/other"
							}
							http.Redirect(w, r, location, status)
						}))
						defer source.Close()
						c, _, _ := newClient(t, source.URL, method == "POST", method)
						res, err := c.Call(context.Background(), apigateway.Request{Provider: "fio", Method: method, Path: "start"})
						if err != nil {
							t.Fatal(err)
						}
						// Redirects are not part of the approved request: even an in-base
						// redirect can change the method or the approved write target.
						if calls.Load() != 0 || leaked.Load() || res.Status != status {
							t.Fatalf("followed redirect: destination calls=%d, status=%d", calls.Load(), res.Status)
						}
					})
				})
			}
		}
	}
}
