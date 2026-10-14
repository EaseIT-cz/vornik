package standins

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// NewTLSIdentity makes a self-signed certificate for 127.0.0.1, ::1 and
// localhost, and returns it with its PEM (to trust it as a root).
func NewTLSIdentity() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "vornik stand-in"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pemBytes, nil
}

// rfc5322 renders a stand-in message as wire bytes.
func rfc5322(m Message) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@standin.invalid>\r\n",
		m.From, m.To, m.Subject, m.Received.UTC().Format(time.RFC1123Z), m.ID)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.Bytes()
}

type literal struct {
	*bytes.Reader
	n int64
}

func (l literal) Size() int64 { return l.n }

// IMAPServer is the IMAP4rev2 stand-in over implicit TLS, on loopback.
type IMAPServer struct {
	Addr  string
	srv   *imapserver.Server
	mu    sync.Mutex
	users map[string]*imapmemserver.User
}

func (s *IMAPServer) append(user, mailbox string, m Message) {
	s.mu.Lock()
	u := s.users[user]
	s.mu.Unlock()
	if u == nil {
		return
	}
	raw := rfc5322(m)
	opts := &imap.AppendOptions{Time: m.Received}
	if !m.Unread {
		opts.Flags = []imap.Flag{imap.FlagSeen}
	}
	_, _ = u.Append(mailbox, literal{bytes.NewReader(raw), int64(len(raw))}, opts)
}

// Close stops the server.
func (s *IMAPServer) Close() error { return s.srv.Close() }

// StartIMAP serves the store over IMAP: one login per user in passwords
// (username = the user's name), INBOX and Sent seeded from the store, and
// every later delivery appended. cert is the TLS identity.
func (m *MailStore) StartIMAP(passwords map[string]string, cert tls.Certificate) (*IMAPServer, error) {
	mem := imapmemserver.New()
	s := &IMAPServer{users: map[string]*imapmemserver.User{}}
	for user, pw := range passwords {
		u := imapmemserver.NewUser(user, pw)
		for _, box := range []string{"INBOX", "Sent"} {
			if err := u.Create(box, nil); err != nil {
				return nil, err
			}
		}
		mem.AddUser(u)
		s.users[user] = u
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	s.Addr = ln.Addr().String()
	s.srv = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger: log.New(io.Discard, "", 0),
	})
	type seed struct {
		user, box string
		msg       Message
	}
	var seeds []seed
	m.mu.Lock()
	for user := range passwords {
		for _, msg := range m.inbox[user] {
			seeds = append(seeds, seed{user, "INBOX", msg})
		}
		for _, msg := range m.sent[user] {
			seeds = append(seeds, seed{user, "Sent", msg})
		}
	}
	m.imap = s
	m.mu.Unlock()
	for _, sd := range seeds {
		s.append(sd.user, sd.box, sd.msg)
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// SMTPServer is the SMTP submission stand-in: plain TCP on loopback, AUTH
// PLAIN, no STARTTLS. An accepted message is recorded in the sender's Sent
// and delivered to each local recipient's INBOX.
type SMTPServer struct {
	Addr     string
	ln       net.Listener
	store    *MailStore
	pw       map[string]string
	mu       sync.Mutex
	received int
}

// Received counts accepted messages.
func (s *SMTPServer) Received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// Close stops the server.
func (s *SMTPServer) Close() error { return s.ln.Close() }

// StartSMTP serves submission for the users in passwords.
func (m *MailStore) StartSMTP(passwords map[string]string) (*SMTPServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &SMTPServer{Addr: ln.Addr().String(), ln: ln, store: m, pw: passwords}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.session(c)
		}
	}()
	return s, nil
}

func (s *SMTPServer) session(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	r := bufio.NewReader(c)
	reply := func(line string) { _, _ = io.WriteString(c, line+"\r\n") }
	reply("220 standin ESMTP")
	user, from := "", ""
	var rcpt []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			reply("250-standin")
			reply("250 AUTH PLAIN")
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			who, ok := s.authPlain(r, reply, strings.TrimSpace(line[len("AUTH PLAIN"):]))
			if !ok {
				return
			}
			if who != "" {
				user = who
			}
		case strings.HasPrefix(verb, "MAIL FROM:"):
			if user == "" {
				reply("530 5.7.0 Authentication required")
				continue
			}
			from, rcpt = addrArg(line[len("MAIL FROM:"):]), nil
			reply("250 OK")
		case strings.HasPrefix(verb, "RCPT TO:"):
			if from == "" {
				reply("503 5.5.1 MAIL first")
				continue
			}
			rcpt = append(rcpt, addrArg(line[len("RCPT TO:"):]))
			reply("250 OK")
		case verb == "DATA":
			if len(rcpt) == 0 {
				reply("503 5.5.1 RCPT first")
				continue
			}
			reply("354 End data with <CR><LF>.<CR><LF>")
			data, err := readData(r)
			if err != nil {
				return
			}
			s.accept(user, from, rcpt, data)
			from, rcpt = "", nil
			reply("250 OK queued")
		case verb == "RSET":
			from, rcpt = "", nil
			reply("250 OK")
		case verb == "NOOP":
			reply("250 OK")
		case verb == "QUIT":
			reply("221 Bye")
			return
		default:
			reply("502 5.5.2 Command not recognized")
		}
	}
}

// authPlain runs AUTH PLAIN (the initial response inline or after a 334).
// It returns the authenticated user ("" when refused) and false when the
// connection ended.
func (s *SMTPServer) authPlain(r *bufio.Reader, reply func(string), arg string) (string, bool) {
	if arg == "" {
		reply("334 ")
		l, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		arg = strings.TrimSpace(l)
	}
	raw, _ := base64.StdEncoding.DecodeString(arg)
	parts := strings.Split(string(raw), "\x00")
	if len(parts) == 3 && parts[2] != "" && s.pw[parts[1]] == parts[2] {
		reply("235 2.7.0 Authentication successful")
		return parts[1], true
	}
	reply("535 5.7.8 Authentication credentials invalid")
	return "", true
}

// readData reads a DATA body up to the lone dot, undoing dot-stuffing.
func readData(r *bufio.Reader) ([]byte, error) {
	var data bytes.Buffer
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if l == ".\r\n" || l == ".\n" {
			return data.Bytes(), nil
		}
		data.WriteString(strings.TrimPrefix(l, "."))
	}
}

func addrArg(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i >= 0 { // drop ESMTP parameters
		s = s[:i]
	}
	return strings.Trim(s, "<>")
}

func (s *SMTPServer) accept(user, from string, rcpt []string, data []byte) {
	msg := Message{From: from, Received: time.Now().UTC()}
	if parsed, err := mail.ReadMessage(bytes.NewReader(data)); err == nil {
		msg.Subject = parsed.Header.Get("Subject")
		if a, err := mail.ParseAddress(parsed.Header.Get("From")); err == nil {
			msg.From = a.Address
		}
		body, _ := io.ReadAll(io.LimitReader(parsed.Body, 1<<20))
		msg.Body = string(body)
	}
	for _, to := range rcpt {
		m := msg
		m.To = to
		s.store.send(user, m)
	}
	s.mu.Lock()
	s.received++
	s.mu.Unlock()
}
