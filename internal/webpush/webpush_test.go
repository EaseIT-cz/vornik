package webpush

// GitHub #163 (agent-administered design part 09 §9.3a): Web Push to
// approver devices. These tests pin the two standards the sender rests on,
// RFC 8291 (message encryption) and RFC 8292 (VAPID), and the endpoint check
// that keeps a device from pointing the daemon at an address of its choosing.

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.ReplaceAll(s, " ", ""))
	require.NoError(t, err)
	return b
}

// RFC 8291 Appendix A: its inputs give its message, byte for byte.
func TestEncrypt_RFC8291AppendixA(t *testing.T) {
	plaintext := b64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	asPriv, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	require.NoError(t, err)
	require.Equal(t, b64(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"),
		asPriv.PublicKey().Bytes())
	uaPublic := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	auth := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")

	got, err := encrypt(plaintext, uaPublic, auth, asPriv, salt)
	require.NoError(t, err)

	header := b64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	ciphertext := b64(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	require.Len(t, header, 86)
	require.Equal(t, append(header, ciphertext...), got)
}

// Encrypt draws a fresh key and salt per message.
func TestEncrypt_FreshKeyAndSaltPerMessage(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(randReader)
	require.NoError(t, err)
	auth := make([]byte, 16)
	a, err := Encrypt([]byte("x"), ua.PublicKey().Bytes(), auth)
	require.NoError(t, err)
	b, err := Encrypt([]byte("x"), ua.PublicKey().Bytes(), auth)
	require.NoError(t, err)
	require.NotEqual(t, a[:16], b[:16], "salt")
	require.NotEqual(t, a[21:86], b[21:86], "sender key")
}

func TestEncrypt_RefusesBadKeys(t *testing.T) {
	_, err := Encrypt([]byte("x"), []byte{4, 1, 2}, make([]byte, 16))
	require.Error(t, err)
	ua, _ := ecdh.P256().GenerateKey(randReader)
	_, err = Encrypt([]byte("x"), ua.PublicKey().Bytes(), make([]byte, 15))
	require.Error(t, err)
}

// RFC 8292: the token verifies with the key the header names, and carries
// aud (the endpoint's origin), exp (12 hours ahead) and sub.
func TestVAPID_HeaderVerifies(t *testing.T) {
	key, err := KeyFromSeed(seedOf("seed"))
	require.NoError(t, err)
	v := NewVAPID(key, "https://vornik.example")
	now := time.Unix(1_800_000_000, 0)
	h, err := v.Header("https://fcm.googleapis.com/fcm/send/abc", now)
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(h, "vapid t="))
	parts := strings.SplitN(strings.TrimPrefix(h, "vapid t="), ", k=", 2)
	require.Len(t, parts, 2)
	require.Equal(t, v.PublicKey(), parts[1])

	seg := strings.Split(parts[0], ".")
	require.Len(t, seg, 3)
	var hdr map[string]string
	require.NoError(t, json.Unmarshal(b64(t, seg[0]), &hdr))
	require.Equal(t, map[string]string{"typ": "JWT", "alg": "ES256"}, hdr)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(b64(t, seg[1]), &claims))
	require.Equal(t, "https://fcm.googleapis.com", claims["aud"])
	require.Equal(t, "https://vornik.example", claims["sub"])
	require.EqualValues(t, now.Add(12*time.Hour).Unix(), claims["exp"])

	pub, err := ecdsa.ParseUncompressedPublicKey(key.Curve, b64(t, parts[1]))
	require.NoError(t, err)
	sig := b64(t, seg[2])
	require.Len(t, sig, 64)
	digest := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	require.True(t, ecdsa.Verify(pub, digest[:], r, s))
}

// One seed, one key: every daemon sharing the master key signs alike.
func TestKeyFromSeed_Deterministic(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 7
	a, err := KeyFromSeed(seed)
	require.NoError(t, err)
	b, err := KeyFromSeed(seed)
	require.NoError(t, err)
	require.True(t, a.Equal(b))
	_, err = KeyFromSeed(make([]byte, 31))
	require.Error(t, err)
}

func TestValidateEndpoint(t *testing.T) {
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://FCM.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/QH0abc",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
	} {
		require.NoError(t, ValidateEndpoint(ok), ok)
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com:8443/fcm/send/abc",
		"https://user@fcm.googleapis.com/fcm/send/abc",
		"https://evil.example/fcm/send/abc",
		"https://fcm.googleapis.com.evil.net/x",
		"https://EVILNOTIFY.windows.com/w",
		"https://notify.windows.com/w",
		"https://notify.windows.com.evil.net/w",
		"https://.notify.windows.com/w",
		"https://10.0.0.1/x",
		"https://fcm.googleapis.com/" + strings.Repeat("a", 1024),
		"",
		"not a url",
	} {
		require.Error(t, ValidateEndpoint(bad), bad)
	}
}

func TestValidateKeys(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(randReader)
	p := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	a := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	require.NoError(t, ValidateKeys(p, a))
	require.NoError(t, ValidateKeys(p+"", base64.URLEncoding.EncodeToString(make([]byte, 16))), "padded base64url is accepted")
	require.Error(t, ValidateKeys(p[:20], a))
	require.Error(t, ValidateKeys(p, base64.RawURLEncoding.EncodeToString(make([]byte, 8))))
	require.Error(t, ValidateKeys("!!", a))
}

func seedOf(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
