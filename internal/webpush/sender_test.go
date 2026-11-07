package webpush

// GitHub #163 §9.3a B.5: the sender posts one encrypted message with the
// VAPID header and classifies the answer. It never follows a redirect: a
// followed redirect would leave the push-service allowlist.

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sub struct{ p256dh, auth string }

func newSub(t *testing.T) sub {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(randReader)
	require.NoError(t, err)
	return sub{
		p256dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		auth:   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
	}
}

func testSender(t *testing.T) *Sender {
	t.Helper()
	key, err := KeyFromSeed(seedOf("sender"))
	require.NoError(t, err)
	s := NewSender(NewVAPID(key, "https://vornik.example"))
	s.client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirect}
	s.validate = func(string) error { return nil } // httptest is loopback http
	return s
}

func TestSender_PostsEncryptedWithHeaders(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	s := testSender(t)
	k := newSub(t)
	out := s.Send(context.Background(), srv.URL+"/push/abc", k.p256dh, k.auth, []byte(`{"title":"t"}`))
	require.Equal(t, OutcomeSent, out.Outcome)
	require.Equal(t, http.StatusCreated, out.Status)
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "aes128gcm", got.Header.Get("Content-Encoding"))
	require.Equal(t, "86400", got.Header.Get("TTL"))
	require.Equal(t, "high", got.Header.Get("Urgency"))
	require.True(t, strings.HasPrefix(got.Header.Get("Authorization"), "vapid t="))
	require.Len(t, body, 86+len(`{"title":"t"}`)+1+16)
}

func TestSender_ClassifiesAnswers(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusCreated, OutcomeSent},
		{http.StatusOK, OutcomeSent},
		{http.StatusNotFound, OutcomeGone},
		{http.StatusGone, OutcomeGone},
		{http.StatusInternalServerError, OutcomeFailed},
		{http.StatusForbidden, OutcomeFailed},
		{http.StatusFound, OutcomeFailed},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.status == http.StatusFound {
				w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			}
			w.WriteHeader(tc.status)
		}))
		k := newSub(t)
		out := testSender(t).Send(context.Background(), srv.URL, k.p256dh, k.auth, []byte("x"))
		srv.Close()
		require.Equal(t, tc.want, out.Outcome, tc.status)
		require.Equal(t, tc.status, out.Status, "a redirect is answered, not followed")
	}
}

func TestSender_ReadsAtMostTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(strings.Repeat("a", 1<<20)))
	}))
	defer srv.Close()
	k := newSub(t)
	s := testSender(t)
	var read int64
	s.onRead = func(n int64) { read = n }
	out := s.Send(context.Background(), srv.URL, k.p256dh, k.auth, []byte("x"))
	require.Equal(t, OutcomeSent, out.Outcome)
	require.LessOrEqual(t, read, int64(maxResponse))
}

func TestSender_RefusesAnEndpointOffTheList(t *testing.T) {
	key, err := KeyFromSeed(seedOf("sender"))
	require.NoError(t, err)
	s := NewSender(NewVAPID(key, "https://vornik.example"))
	k := newSub(t)
	out := s.Send(context.Background(), "https://evil.example/x", k.p256dh, k.auth, []byte("x"))
	require.Equal(t, OutcomeFailed, out.Outcome)
	require.ErrorIs(t, out.Err, ErrEndpoint)
}

// The production client refuses redirects (NewGuardedClient alone follows
// up to 10).
func TestNewSender_ClientRefusesRedirects(t *testing.T) {
	key, _ := KeyFromSeed(seedOf("sender"))
	s := NewSender(NewVAPID(key, "https://vornik.example"))
	require.NotNil(t, s.client.CheckRedirect)
	require.ErrorIs(t, s.client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	require.Equal(t, 10*time.Second, s.client.Timeout)
}
