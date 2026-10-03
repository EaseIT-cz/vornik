package approverdevice

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The page a tapped link asked for, kept across pairing (agent-administered
// design §9.2a): a link opened in a browser that is not paired must land on
// its request once pairing completes, not on the list.

// NextCookieName holds the page asked for while a further device waits for
// its enrollment approval; scoped like the claim cookie.
const NextCookieName = "vornik_pair_next"

var nextPath = regexp.MustCompile(`^/ui/approve/[A-Za-z0-9_/-]{0,120}$`)

// validNext returns p when it is a path under /ui/approve/ and nothing else
// (no scheme, no host, no "//", no ".."), or "". Every use re-validates.
func validNext(p string) string {
	if !nextPath.MatchString(p) || strings.Contains(p, "//") {
		return ""
	}
	return p
}

// pairURL is where RequireDevice sends an unpaired GET.
func pairURL(path string) string {
	if n := validNext(path); n != "" && n != "/ui/approve/" {
		return "/ui/pair?next=" + url.QueryEscape(n)
	}
	return "/ui/pair"
}

func setNextCookie(w http.ResponseWriter, r *http.Request, next string) {
	http.SetCookie(w, &http.Cookie{Name: NextCookieName, Value: url.QueryEscape(next), Path: "/ui/pair", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: int(ClaimTTL.Seconds())})
}

func clearNextCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: NextCookieName, Value: "", Path: "/ui/pair", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r), MaxAge: -1})
}

// nextFromCookie is the validated page the waiting device asked for, or "".
func nextFromCookie(r *http.Request) string {
	c, err := r.Cookie(NextCookieName)
	if err != nil {
		return ""
	}
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return validNext(v)
}

// orDefault is next, or the list when none was asked for.
func orDefault(next string) string {
	if next == "" {
		return "/ui/approve/"
	}
	return next
}

// pairingHelp backs the explanation and the reopen buttons on the pair and
// wait pages.
type pairingHelp struct {
	Next           string
	Chrome, Safari template.URL
	// Address is the page to type or paste into the paired browser: the
	// fallback where a reopen scheme does not work (x-safari-https is
	// absent on iOS 16; review 20261003-601e F2). Shown on every browser.
	Address string
}

// help builds the reopen links from the configured origin and the validated
// next, never from a request header; only for an iOS browser, where the
// schemes exist.
func (s *Service) help(r *http.Request, next string) pairingHelp {
	h := pairingHelp{Next: next}
	u, err := url.Parse(s.origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return h
	}
	h.Address = u.Scheme + "://" + u.Host + orDefault(next)
	ua := r.UserAgent()
	if !strings.Contains(ua, "iPhone") && !strings.Contains(ua, "iPad") && !strings.Contains(ua, "iPod") {
		return h
	}
	target := u.Host + orDefault(next)
	if u.Scheme == "https" {
		h.Chrome = template.URL("googlechromes://" + target)  // #nosec G203 -- host from config, path validated
		h.Safari = template.URL("x-safari-https://" + target) // #nosec G203 -- host from config, path validated
	} else {
		h.Chrome = template.URL("googlechrome://" + target)  // #nosec G203 -- host from config, path validated
		h.Safari = template.URL("x-safari-http://" + target) // #nosec G203 -- host from config, path validated
	}
	return h
}
