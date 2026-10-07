package secrets

import (
	"strings"
	"testing"
)

// Issue #69, 2026-10-06: preserve canonical Google resource IDs without
// allowing a trusted hostname to suppress real credentials elsewhere.
func TestGoogleDocumentURLsPreserveIDsOnly(t *testing.T) {
	const id = "1aB2cD3eF4gH5iJ6kL7mN8oP9qR0sT_uVwXyZabcdEfGh"
	d := realDetector(t)
	for _, suffix := range []string{"", "/edit", "/view", "/preview", "/", "/edit/", "?usp=sharing", "#heading=h.short"} {
		for _, host := range []string{"docs.google.com", "drive.google.com", "sheets.google.com", "DOCS.GOOGLE.COM"} {
			text := "Tracker [link](https://" + host + "/spreadsheets/d/" + id + suffix + ")"
			if out := string(Redact([]byte(text), d.Scan([]byte(text)))); out != text {
				t.Errorf("document ID destroyed: %s", out)
			}
		}
	}
	for _, text := range []string{
		"https://docs.google.com.evil.com/spreadsheets/d/" + id + "/edit",
		"https://evil-docs.google.com/spreadsheets/d/" + id,
		"https://docs.google.com@evil.com/spreadsheets/d/" + id,
		"https://user:password@docs.google.com/spreadsheets/d/" + id,
		"https://docs.google.com:443/spreadsheets/d/" + id,
		"http://docs.google.com/spreadsheets/d/" + id,
		"https://docs.google.com/unknown/" + id,
		"https://docs.google.com/spreadsheets/d/" + id + "/arbitrary",
		"https://docs.google.com/spreadsheets/d/" + id + "%2Fedit",
		id,
	} {
		if out := string(Redact([]byte(text), d.Scan([]byte(text)))); out == text {
			t.Errorf("unrecognized URL/token escaped entropy scan: %s", text)
		}
	}
}

func TestGoogleDocumentURLsStillRedactCredentials(t *testing.T) {
	const id = "1aB2cD3eF4gH5iJ6kL7mN8oP9qR0sT_uVwXyZabcdEfGh"
	const key = "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	d := realDetector(t)
	base := "https://docs.google.com/spreadsheets/d/" + id + "/edit"
	for _, text := range []string{
		base + "?token=" + id,
		base + "?opaque=" + id,
		base + "#token=" + id,
		base + "#" + id,
		base + " API key " + key,
		"https://docs.google.com/document/d/" + key + "/edit",
		"https://docs.google.com/document/d/AIzaAbCdEfGhIjKlMnOpQrStUvWxYz012345678/view",
	} {
		out := string(Redact([]byte(text), d.Scan([]byte(text))))
		if out == text || !strings.Contains(out, "[REDACTED:") {
			t.Errorf("credential escaped scan: %s", out)
		}
		if strings.Contains(out, key) {
			t.Errorf("vendor key escaped scan: %s", out)
		}
		if strings.HasPrefix(text, base) && !strings.HasPrefix(out, base) {
			t.Errorf("credential redaction destroyed safe path: %s", out)
		}
	}
}
