package secrets

import (
	"net/url"
	"regexp"
	"strings"
)

var documentURLCandidate = regexp.MustCompile(`(?i)https?://[^\s<>"'` + "`" + `()\[\]{}]+`)
var googleDocumentPath = regexp.MustCompile(`^/(?:(?:document|spreadsheets|presentation|file)/d/[A-Za-z0-9_-]+(?:/(?:edit|view|preview))?|drive/folders/[A-Za-z0-9_-]+)/?$`)

// googleDocumentURLSpans recognizes resource paths, never credential-bearing
// queries or fragments. These spans exempt ONLY the entropy fallback; regex
// credential findings are preserved. See issue #69 and the detector LLD.
func googleDocumentURLSpans(text []byte) [][2]int {
	var spans [][2]int
	for _, m := range documentURLCandidate.FindAllIndex(text, -1) {
		raw := strings.TrimRight(string(text[m[0]:m[1]]), ".,;!")
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawPath != "" || !googleDocumentPath.MatchString(u.Path) {
			continue
		}
		// Compare Host, not Hostname: even an explicit :443 fails closed.
		switch strings.ToLower(u.Host) {
		case "docs.google.com", "drive.google.com", "sheets.google.com":
		default:
			continue
		}
		end := len(raw)
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			end = i
		}
		spans = append(spans, [2]int{m[0], m[0] + end})
	}
	return spans
}

func withinGoogleDocumentURL(spans [][2]int, start, end int) bool {
	for _, span := range spans {
		if start >= span[0] && end <= span[1] {
			return true
		}
	}
	return false
}
