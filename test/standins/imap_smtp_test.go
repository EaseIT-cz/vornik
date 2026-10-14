package standins

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/email"
)

// GitHub #337 (2026-10-12): the IMAP and SMTP stand-ins are read and fed by
// the email channel's own production clients (internal/email): the IMAP
// client dials implicit TLS against the system roots, so the run's
// self-signed identity is made a system root for this test binary through
// SSL_CERT_FILE, set before anything loads the pool.

var testTLS tls.Certificate

func TestMain(m *testing.M) {
	cert, pemBytes, err := NewTLSIdentity()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "standins-ca")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("SSL_CERT_FILE", path)
	testTLS = cert
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func TestIMAP_TheChannelClientFetchesUnseenAndMarksSeen(t *testing.T) {
	store := seededMail()
	srv, err := store.StartIMAP(map[string]string{"alice": "pw-a", "bob": "pw-b"}, testTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := email.IMAPDialConfig{Host: "127.0.0.1", Port: portOf(t, srv.Addr), Username: "alice", Password: "pw-a"}
	cli := email.NewIMAPClient()
	if err := cli.Connect(ctx, cfg); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cli.Close() }()
	got, err := cli.FetchUnseen(ctx)
	if err != nil || len(got) != 1 || !strings.Contains(string(got[0].Body), "Subject: Invoice 4411") ||
		!strings.Contains(string(got[0].Body), "BODY-CANARY-A") {
		t.Fatalf("unseen: %v %+v", err, got)
	}
	if err := cli.MarkSeen(ctx, got[0].UID); err != nil {
		t.Fatal(err)
	}
	if again, err := cli.FetchUnseen(ctx); err != nil || len(again) != 0 {
		t.Fatalf("after MarkSeen: %v %d", err, len(again))
	}
	// Mail delivered after the start reaches IMAP too (one store).
	store.Deliver("alice", Message{From: "late@acme.example", Subject: "Late", Body: "x", Received: clock, Unread: true})
	if late, err := cli.FetchUnseen(ctx); err != nil || len(late) != 1 || !strings.Contains(string(late[0].Body), "Subject: Late") {
		t.Fatalf("a later delivery: %v %d", err, len(late))
	}

	// Bob sees only his own mail; a wrong password is refused.
	bob := email.NewIMAPClient()
	if err := bob.Connect(ctx, email.IMAPDialConfig{Host: "127.0.0.1", Port: cfg.Port, Username: "bob", Password: "pw-b"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bob.Close() }()
	if b, err := bob.FetchUnseen(ctx); err != nil || len(b) != 1 || strings.Contains(string(b[0].Body), "CANARY-A") {
		t.Fatalf("bob: %v %+v", err, b)
	}
	if err := email.NewIMAPClient().Connect(ctx, email.IMAPDialConfig{Host: "127.0.0.1", Port: cfg.Port, Username: "alice", Password: "nope"}); err == nil {
		t.Fatal("a wrong password logged in")
	}
}

func TestSMTP_TheChannelSenderDeliversIntoTheStore(t *testing.T) {
	store := seededMail()
	srv, err := store.StartSMTP(map[string]string{"alice": "pw-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	payload, _, err := email.RenderRFC5322(email.OutboundMessage{From: "alice@home.example", To: "bob@work.example", Subject: "Lunch?", Body: "At noon.", Date: clock})
	if err != nil {
		t.Fatal(err)
	}
	send := email.NewNetSMTPSender("localhost", portOf(t, srv.Addr), "alice", "pw-a")
	if err := send.Send(context.Background(), email.SMTPSendRequest{From: "Alice <alice@home.example>", To: []string{"bob@work.example"}, Payload: payload}); err != nil {
		t.Fatalf("send: %v", err)
	}
	in := store.Inbox("bob")
	if last := in[len(in)-1]; last.Subject != "Lunch?" || last.From != "alice@home.example" || !strings.Contains(last.Body, "At noon.") {
		t.Fatalf("bob's INBOX: %+v", last)
	}
	if s := store.Sent("alice"); len(s) != 1 {
		t.Fatalf("alice's Sent: %+v", s)
	}
	bad := email.NewNetSMTPSender("localhost", portOf(t, srv.Addr), "alice", "nope")
	if err := bad.Send(context.Background(), email.SMTPSendRequest{From: "alice@home.example", To: []string{"bob@work.example"}, Payload: payload}); err == nil {
		t.Fatal("a wrong password sent mail")
	}
	if got := srv.Received(); got != 1 {
		t.Fatalf("received = %d", got)
	}
}
