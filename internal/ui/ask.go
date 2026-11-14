package ui

import (
	"net/http"
	"net/url"
	"unicode/utf8"

	"vornik.io/vornik/internal/api"
)

// askHandler serves /ui/ask, the Ask door (ui-design-refresh-design.md
// §13.2, #755). It reads only configuration: the dispatcher project, the
// one /ui/setup's last step links to. If that id is plain, registered and
// open to the session, it redirects to /ui/projects/{id}/chat; otherwise to
// /ui/projects, so the caller picks an assistant. The target is always a
// server-built /ui/ path, so it cannot redirect off the console. Unauthenticated requests never get here: the UI session
// middleware sends them to login first.
func askHandler(dispatcher func() string, canOpen func(*http.Request, string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := "/ui/projects"
		if id := dispatcher(); id != "" && sayHelloProjectIDPattern.MatchString(id) && canOpen(r, id) {
			target = "/ui/projects/" + id + "/chat"
			// The home page's Ask box (#756, design §14.2): the question is
			// percent-encoded and cut to askQuestionMaxRunes; the chat page
			// only pre-fills its composer with it.
			if q := truncateRunes(r.URL.Query().Get("q"), askQuestionMaxRunes); q != "" {
				target += "?" + url.Values{"q": {q}}.Encode()
			}
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

// askQuestionMaxRunes caps the question carried from the home page's Ask box.
const askQuestionMaxRunes = 2000

// truncateRunes cuts s to at most n runes, never inside a UTF-8 sequence.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

// Ask is the Server's /ui/ask handler: the dispatcher id from config,
// checked against the registry (sayHelloProjectID) and the session's
// project scope (the check every project page applies).
func (s *Server) Ask(w http.ResponseWriter, r *http.Request) {
	askHandler(func() string {
		if cfg := s.onboardingDetector.Config; cfg != nil {
			return s.sayHelloProjectID(cfg.Telegram.DispatcherProjectID)
		}
		return ""
	}, api.RequestAllowsProject)(w, r)
}
