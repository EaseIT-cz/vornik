package forge

// GitHub #838 (github-app-channel design, amendment 2026-11-19), a port of
// community PR EaseIT-cz/vornik#21 by Veaceslav Mindru: every text a forge
// review publishes is scanned, by one function, and a secret-shaped span is
// not published while the rest is.

import (
	"context"
	"encoding/base64"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/secrets"
)

var forgeReviewRedactions = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "forge_review_redactions_total",
	Help: "Secret-shaped spans masked in a published forge review (GitHub #838), by path: review, comment, grounded (a withheld finding); review_withheld and comment_withheld count texts withheld because no detector was wired.",
}, []string{"path"})

// The Basic Auth rules the shared detector lacks (measured 2026-11-19: it has
// no finding on "Authorization: Basic …" or a detached base64 user:pass). Each
// rule's first group is the span to mask.
var (
	basicAuthHeader = regexp.MustCompile(`(?i)\bauthorization\s*:\s*basic\s+([A-Za-z0-9+/]{8,}={0,2})`)
	// Standard and URL-safe alphabets (review 1d6a M5).
	detachedBase64 = regexp.MustCompile(`(?:^|[^A-Za-z0-9+/_-])([A-Za-z0-9+/_-]{8,}={0,2})(?:$|[^A-Za-z0-9+/_=-])`)
	decodedBasic   = regexp.MustCompile(`(?i)\b(?:decoded\s+(?:as|to)|basic\s+auth(?:entication)?(?:\s+credentials?)?)\s*[:=]?\s*([A-Za-z0-9_.-]{1,64}:[A-Za-z0-9!@#$%^&*()_+={}\[\]:;,.?/-]{3,128})`)
)

// withheldUnscanned replaces a review that cannot be scanned.
const withheldUnscanned = "(The review was withheld: it could not be scanned for secrets.)"

// publicationFindings is every secret-shaped span in text: the shared
// detector's findings of every type, plus the Basic Auth rules, sorted and
// with overlaps joined (secrets.Redact skips a span that starts inside the
// previous one). A nil detector is the caller's to refuse.
func publicationFindings(d secrets.Detector, text string) []secrets.Finding {
	var out []secrets.Finding
	if d != nil {
		out = append(out, d.Scan([]byte(text))...)
	}
	for _, m := range basicAuthHeader.FindAllStringSubmatchIndex(text, -1) {
		out = append(out, secrets.Finding{Type: "basic_auth", Start: m[2], End: m[3]})
	}
	for _, m := range decodedBasic.FindAllStringSubmatchIndex(text, -1) {
		out = append(out, secrets.Finding{Type: "basic_auth", Start: m[2], End: m[3]})
	}
	for _, m := range detachedBase64.FindAllStringSubmatchIndex(text, -1) {
		decoded, ok := decodeUserPass(text[m[2]:m[3]])
		if !ok {
			continue
		}
		out = append(out, secrets.Finding{Type: "basic_auth", Start: m[2], End: m[3]})
		// The decoded form is masked wherever it appears, with or without a
		// "decoded as" phrase near it (review 1d6a H2).
		for at := 0; ; {
			i := strings.Index(text[at:], decoded)
			if i < 0 {
				break
			}
			out = append(out, secrets.Finding{Type: "basic_auth", Start: at + i, End: at + i + len(decoded)})
			at += i + len(decoded)
		}
	}
	return joinSpans(out)
}

// decodeUserPass decodes tok (standard or URL-safe base64, padded or not)
// and reports whether it is printable text with exactly one colon, neither
// first nor last: the shape of a Basic credential.
func decodeUserPass(tok string) (string, bool) {
	var b []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err = enc.DecodeString(tok); err == nil {
			break
		}
	}
	if err != nil {
		return "", false
	}
	s := string(b)
	i := strings.IndexByte(s, ':')
	// A password of at least 3 characters, as decodedBasic requires (review
	// 82a3 L1): "abc:de" or a ratio is not a credential.
	if i <= 0 || len(s)-i-1 < 3 || strings.Count(s, ":") != 1 {
		return "", false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	return s, true
}

// joinSpans sorts spans by start and joins overlapping ones.
func joinSpans(fs []secrets.Finding) []secrets.Finding {
	if len(fs) < 2 {
		return fs
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Start < fs[j].Start })
	out := []secrets.Finding{fs[0]}
	for _, f := range fs[1:] {
		last := &out[len(out)-1]
		if f.Start < last.End {
			if f.End > last.End {
				last.End = f.End
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// forPublication is the text that may be published: each secret-shaped span
// masked, or the withheld notice when there is no detector to scan with.
// Logs the count and types, never a value.
func (h *PostReviewHandler) forPublication(ctx context.Context, path, text string) string {
	if h.detector == nil {
		forgeReviewRedactions.WithLabelValues(path + "_withheld").Inc() // review 1d6a: visible, not silent
		zerolog.Ctx(ctx).Warn().Str("path", path).
			Msg("forge.post_review: no secret detector; the review text is withheld (#838)")
		return withheldUnscanned
	}
	fs := publicationFindings(h.detector, text)
	if len(fs) == 0 {
		return text
	}
	types := make([]string, 0, len(fs))
	for _, f := range fs {
		types = append(types, f.Type)
	}
	forgeReviewRedactions.WithLabelValues(path).Add(float64(len(fs)))
	zerolog.Ctx(ctx).Warn().Str("path", path).Int("spans", len(fs)).Strs("types", types).
		Msg("forge.post_review: secret-shaped text masked before publishing (#838)")
	return string(secrets.Redact([]byte(text), fs))
}
