package service

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// GitHub #581 D2a (enterprise-packaging-design.md, amendment 2026-10-29):
// the managed upgrade rolls back only a start that never served, and takes
// the unit's READY=1 for "served". So a start must fail before any listener
// serves. At 540e26686 the public listener served from a goroutine while a
// port clash on the operations listener reached errCh only after READY=1.
func TestStartListeners581_APortClashFailsBeforeAnyListenerServes(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	publicAddr := freeAddr(t)
	served := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served <- struct{}{} })
	c := &Container{
		Config:           &config.Config{},
		HTTPServer:       &http.Server{Addr: publicAddr, Handler: handler, ReadHeaderTimeout: time.Second},
		OperationsServer: &http.Server{Addr: taken.Addr().String(), Handler: handler, ReadHeaderTimeout: time.Second},
	}
	errCh := make(chan error, 4)
	if err := c.startListeners(context.Background(), errCh); err == nil {
		t.Fatal("startListeners = nil with the operations port taken; want the bind error")
	}
	// The public listener must not be serving (nor even bound).
	conn, err := net.DialTimeout("tcp", publicAddr, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the public listener accepts connections after a failed start")
	}
	select {
	case <-served:
		t.Fatal("a request was served by a start that failed")
	default:
	}
}

// The happy path: both listeners bound and serving when startListeners
// returns, so READY=1 after it is true.
func TestStartListeners581_BothListenersServeOnReturn(t *testing.T) {
	publicAddr, opsAddr := freeAddr(t), freeAddr(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	c := &Container{
		Config:           &config.Config{},
		HTTPServer:       &http.Server{Addr: publicAddr, Handler: ok, ReadHeaderTimeout: time.Second},
		OperationsServer: &http.Server{Addr: opsAddr, Handler: ok, ReadHeaderTimeout: time.Second},
	}
	t.Cleanup(func() { _ = c.HTTPServer.Close(); _ = c.OperationsServer.Close() })
	if err := c.startListeners(context.Background(), make(chan error, 4)); err != nil {
		t.Fatalf("startListeners = %v", err)
	}
	for _, addr := range []string{publicAddr, opsAddr} {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("GET %s: %v", addr, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("GET %s = %d", addr, resp.StatusCode)
		}
	}
}

// A fallible step after the binds (here the relay-ingress init, which refuses
// without an API server) closes what was bound: no listener serves a start
// that failed (review 0f9d #6).
func TestStartListeners581_AFailureAfterTheBindsClosesThem(t *testing.T) {
	publicAddr, opsAddr := freeAddr(t), freeAddr(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cfg := &config.Config{}
	cfg.Node.RelayIngress.Addr = freeAddr(t)
	c := &Container{
		Config:           cfg,
		HTTPServer:       &http.Server{Addr: publicAddr, Handler: ok, ReadHeaderTimeout: time.Second},
		OperationsServer: &http.Server{Addr: opsAddr, Handler: ok, ReadHeaderTimeout: time.Second},
	}
	if !c.capabilities().RunWorkers {
		t.Skip("the default node profile does not run workers; the relay init would not run")
	}
	if err := c.startListeners(context.Background(), make(chan error, 4)); err == nil {
		t.Fatal("startListeners = nil; want the relay-ingress init error")
	}
	for _, addr := range []string{publicAddr, opsAddr} {
		if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
			_ = conn.Close()
			t.Errorf("%s accepts connections after a failed start", addr)
		}
	}
}
