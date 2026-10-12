package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// setArtifactBody replaces the fixture's bytes and keeps size and hash true.
func setArtifactBody(a *persistence.Artifact, o *artifactBytesOpener, body []byte) {
	o.body = body
	size := int64(len(body))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	a.SizeBytes, a.ContentHashSHA256 = &size, &hash
}

func utf16Bytes(s string, bigEndian, bom bool) []byte {
	var out []byte
	if bom {
		if bigEndian {
			out = append(out, 0xfe, 0xff)
		} else {
			out = append(out, 0xff, 0xfe)
		}
	}
	for _, u := range utf16.Encode([]rune(s)) {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

// audit 2026-10-12, finding E, hardened after the lead's security review
// (parser differential): the decision to scan must not rest on anything the
// agent controls (name, MIME, a sniffed prefix, an early NUL, UTF-16, gzip).
// Every byte a broker view serves is scanned: a binary payload through its
// raw, UTF-16LE, UTF-16BE and one-level gzip views before a link is minted,
// and again at redemption.
func TestCompanionArtifactBrokerBinaryIsScanned(t *testing.T) {
	secret := "Authorization: Bearer sk-" + strings.Repeat("a", 48)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("notes\n" + secret + "\n"))
	require.NoError(t, zw.Close())
	cases := map[string][]byte{
		"leading_nul":     append([]byte{0, 0, 0}, []byte(secret)...),
		"binary_prefix":   append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}, []byte(" "+secret)...),
		"utf16le_bom":     utf16Bytes(secret, false, true),
		"utf16be_no_bom":  utf16Bytes(secret, true, false),
		"utf16le_odd":     append([]byte{0xff}, utf16Bytes(secret, false, false)...),
		"gzip":            gz.Bytes(),
		"secret_mid_file": append(append(bytes.Repeat([]byte{0xde, 0xad, 0x00}, 5000), []byte(secret)...), 0xff),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, raw, a, opener, _ := artifactFixture(t, true)
			setArtifactBody(a, opener, body)
			out, bad := artifactRead(t, s, raw)
			require.True(t, bad, "%s: a secret in served bytes must be refused, got %v", name, out)
			require.Contains(t, out["error"], "egress_secret")
		})
	}
	// review-20261012-02e9 G1, G2: a gzip the scan cannot read whole is
	// refused, because the recipient can decompress what the scan did not see.
	var big bytes.Buffer
	bw, _ := gzip.NewWriterLevel(&big, gzip.BestCompression)
	_, _ = bw.Write(make([]byte, 33<<20))
	_, _ = bw.Write([]byte(secret))
	require.NoError(t, bw.Close())
	unscannable := map[string][]byte{
		"gzip_corrupt":   append([]byte{0x1f, 0x8b, 0x08, 0x00}, bytes.Repeat([]byte{0xab}, 64)...),
		"gzip_truncated": gz.Bytes()[:len(gz.Bytes())-10],
		"gzip_over_32mb": big.Bytes(),
	}
	for name, body := range unscannable {
		t.Run(name, func(t *testing.T) {
			s, _, raw, a, opener, _ := artifactFixture(t, true)
			setArtifactBody(a, opener, body)
			out, bad := artifactRead(t, s, raw)
			require.True(t, bad, "%s: an unscannable gzip must be refused, got %v", name, out)
			require.Contains(t, out["error"], "egress_secret")
		})
	}
	t.Run("clean_binary_passes", func(t *testing.T) {
		s, _, raw, a, opener, _ := artifactFixture(t, true)
		setArtifactBody(a, opener, append([]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"), bytes.Repeat([]byte{0x00, 0x13, 0x37}, 1000)...))
		out, bad := artifactRead(t, s, raw)
		require.False(t, bad, "%v", out)
		require.Equal(t, "binary", out["kind"])
	})
	t.Run("scanner_missing_withholds_binary", func(t *testing.T) {
		s, _, raw, _, _, _ := artifactFixture(t, true)
		s.egress = nil
		out, bad := artifactRead(t, s, raw)
		require.True(t, bad, "no scanner: a broker binary must be withheld (fail closed)")
		require.Contains(t, out["error"], "egress_secret")
	})
	// review-20261012-2719 N2: a clean gzip still gets a link.
	t.Run("clean_gzip_passes", func(t *testing.T) {
		var clean bytes.Buffer
		cw := gzip.NewWriter(&clean)
		_, _ = cw.Write([]byte("an ordinary report with no credentials in it\n"))
		require.NoError(t, cw.Close())
		s, _, raw, a, opener, _ := artifactFixture(t, true)
		setArtifactBody(a, opener, clean.Bytes())
		out, bad := artifactRead(t, s, raw)
		require.False(t, bad, "%v", out)
		require.Equal(t, "binary", out["kind"])
	})
	// review-20261012-2719 N1: a saturated scan is refused as busy, never as
	// an egress_secret finding.
	t.Run("busy_is_not_a_finding", func(t *testing.T) {
		s, _, raw, _, _, _ := artifactFixture(t, true)
		s.artifactSlotsInit()
		for i := 0; i < cap(s.artifactDownloads.slots); i++ {
			s.artifactDownloads.slots <- struct{}{}
		}
		out, bad := artifactRead(t, s, raw)
		require.True(t, bad)
		require.Contains(t, out["error"], "capacity reached")
		require.NotContains(t, out["error"], "egress_secret")
	})
	t.Run("redemption_rescans", func(t *testing.T) {
		s, _, raw, a, opener, _ := artifactFixture(t, true)
		out, bad := artifactRead(t, s, raw)
		require.False(t, bad)
		// The stored bytes change after the link was minted (same artifact id).
		setArtifactBody(a, opener, append([]byte{0}, []byte(secret)...))
		router := NewRouter(s, &config.Config{})
		w := httptest.NewRecorder()
		router.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, out["download_url"].(string), nil))
		require.Equal(t, http.StatusForbidden, w.Code)
		require.NotContains(t, w.Body.String(), "sk-")
	})
	t.Run("operator_view_unchanged", func(t *testing.T) {
		// A non-broker view never scanned and still does not (15-artifact-store §11).
		s, _, raw, a, opener, _ := artifactFixture(t, false)
		setArtifactBody(a, opener, append([]byte{0}, []byte(secret)...))
		out, bad := artifactRead(t, s, raw)
		require.False(t, bad, "%v", out)
		require.Equal(t, "binary", out["kind"])
	})
}
