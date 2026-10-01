package companionpush

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// maxTokenLen bounds notify.token (design §7a).
const maxTokenLen = 512

// ValidateNotifyURL is delegate's set-time check of notify.url (design §7a):
// an absolute http(s) URL, not localhost, and — when the host is a literal
// address — one the push may reach under the project's allowlist. A
// hostname passes here; the dial guard judges its resolved address at
// connect time, which is the authority. It is deliberately separate from
// a2a.ValidateWebhookURL, which stays refuse-all-private for A2A.
func ValidateNotifyURL(raw string, allowed []netip.Prefix) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("notify.url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("notify.url is not an absolute URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("notify.url scheme must be http or https")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("notify.url has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("notify.url may not target localhost")
	}
	if a, err := netip.ParseAddr(host); err == nil && !reachAllowed(a, allowed) {
		return fmt.Errorf("notify.url targets %s, which is not public and not in the project's companion_push.allowed_cidrs", a)
	}
	return nil
}

// ValidateNotifyToken bounds notify.token: at most 512 printable ASCII
// characters, since it is sent back as a header value.
func ValidateNotifyToken(tok string) error {
	if len(tok) > maxTokenLen {
		return fmt.Errorf("notify.token is longer than %d characters", maxTokenLen)
	}
	for _, r := range tok {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("notify.token must be printable ASCII without spaces")
		}
	}
	return nil
}
