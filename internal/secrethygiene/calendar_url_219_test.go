package secrethygiene

import (
	"strings"
	"testing"
)

// GitHub #219 (config-assistant design §17). The reference host's assistant
// project keeps two Google Calendar secret addresses in its workspace
// PROJECT_CONTEXT.md, as markdown list items. ScanText read only `key: value`
// lines, so it found nothing, and the URLs reached the assistant's model call,
// the judge prompt and the stored proposal diff. Anyone holding such a URL
// reads the calendar: it is a credential.

// Tokens are built at run time so no literal tokenised URL sits in the tree.
var tok219 = strings.Repeat("a1b2c3d4", 4) // 32 chars, the length of a Google secret

func TestScanText_219_FindsEachTokenisedCalendarShape(t *testing.T) {
	for name, line := range map[string]string{
		"google markdown item": "- family calendar: <https://calendar.google.com/calendar/ical/someone%40example.com/private-" + tok219 + "/basic.ics>",
		"google prose":         "Read https://calendar.google.com/calendar/ical/abc%40group.calendar.google.com/private-" + tok219 + "/basic.ics for events.",
		"outlook published":    "* https://outlook.office365.com/owa/calendar/0f0e@example.com/" + tok219 + "/calendar.ics",
		"outlook live":         "feed https://outlook.live.com/owa/calendar/00000000-0000-0000-0000-000000000000/" + tok219 + "/cid-1/calendar.ics",
		"icloud webcal":        "webcal://p42-caldav.icloud.com/published/2/" + tok219 + tok219,
		"outlook encoded id":   "https://outlook.office365.com/owa/calendar/a%2Fb@example.com/" + tok219 + "/calendar.ics.",
		"google markdown link": "[cal](https://calendar.google.com/calendar/ical/x/private-" + tok219 + "/basic.ics).",
	} {
		fs := ScanText("# Context\n\n" + line + "\n")
		if len(fs) != 1 {
			t.Fatalf("%s: want 1 finding, got %v", name, fs)
		}
		f := fs[0]
		if f.Kind != KindRawSecret || f.Key != "tokenised calendar URL" || !strings.Contains(f.Detail, "line 3:") {
			t.Fatalf("%s: finding %+v", name, f)
		}
		if strings.Contains(f.Detail, tok219) || strings.Contains(f.Detail, "://") || strings.Contains(f.Key, tok219) {
			t.Fatalf("%s: the finding must not carry the URL: %+v", name, f)
		}
	}
}

func TestScanText_219_NearMissesAreNotFindings(t *testing.T) {
	for name, line := range map[string]string{
		"google public":        "https://calendar.google.com/calendar/ical/en.usa%23holiday%40group.v.calendar.google.com/public/basic.ics",
		"google placeholder":   "https://calendar.google.com/calendar/ical/<addr>/private-<token>/basic.ics",
		"google short token":   "https://calendar.google.com/calendar/ical/x/private-abc/basic.ics",
		"google embed page":    "https://calendar.google.com/calendar/embed?src=" + tok219,
		"other host private":   "https://example.com/calendar/ical/x/private-" + tok219 + "/basic.ics",
		"outlook not owa":      "https://outlook.office365.com/mail/" + tok219 + "/calendar.ics",
		"icloud not published": "https://www.icloud.com/calendar/" + tok219,
		"placeholder token":    "https://calendar.google.com/calendar/ical/x/private-your_calendar_token_here/basic.ics",
	} {
		if fs := ScanText(line); len(fs) != 0 {
			t.Fatalf("%s: want no finding, got %v", name, fs)
		}
	}
}

// A `key: value` line the first pass already flags is one finding, not two.
func TestScanText_219_NoDoubleFinding(t *testing.T) {
	fs := ScanText("ics_url: https://calendar.google.com/calendar/ical/x/private-" + tok219 + "/basic.ics\n")
	if len(fs) != 1 || fs[0].Key != "ics_url" {
		t.Fatalf("want the one key finding, got %v", fs)
	}
}

// A diff line adding the URL is found, so the judge never receives it.
func TestScanText_219_DiffLine(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n # ctx\n+- https://calendar.google.com/calendar/ical/x/private-" + tok219 + "/basic.ics\n"
	if fs := ScanText(diff); len(fs) != 1 {
		t.Fatalf("want 1 finding on the added line, got %v", fs)
	}
}
