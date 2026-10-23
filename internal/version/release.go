package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Calendar release grammar and order — loader-validator agreement design,
// 2026-10-17 amendment (#371).
//
// The ONE place a release string is parsed. Before it, `vornikctl config keys
// --since` compared releases as Go strings ("2026.10.0" < "2026.9.8", because
// '1' < '9'), and telemetry kept a private regex for the same shape. Two
// grammars for one concept is how one of them ends up wrong.
//
// The accepted shape is what the daemon reports for a build of a tagged commit
// (`git describe --tags --always --dirty`, from the Makefile):
//
//	YYYY.M.P[-(alpha|beta|rc)[.]N][-N-g<hex>][-dirty]
//
// Anything else — the dev+g<rev> VCS fallback, "unstamped", a bare commit hash,
// "-dev", "+metadata" — names a commit rather than a position in the release
// order, so it does not parse and is never guessed into an order.
var releasePattern = regexp.MustCompile(
	`^([0-9]{4})\.([0-9]{1,2})\.([0-9]{1,3})` + // YYYY.M.P
		`(?:-(alpha|beta|rc)\.?([0-9]{1,3}))?` + // pre-release
		`(?:-([0-9]+)-g([0-9a-f]+))?` + // git describe: commits since the tag
		`(-dirty)?$`)

// Release is a parsed calendar release.
type Release struct {
	Year, Month, Patch int
	// Pre is "alpha", "beta" or "rc" for a pre-release, "" otherwise; PreNum is
	// its number.
	Pre    string
	PreNum int
	// Commits is the git-describe count of commits after the tag; 0 for the
	// tagged commit itself.
	Commits int
	// Describe is true when the string carried a -N-g<hex> part.
	Describe bool
	Dirty    bool
}

// ParseRelease parses s (surrounding space ignored). ok is false for anything
// that is not a calendar release in the grammar above.
func ParseRelease(s string) (Release, bool) {
	m := releasePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Release{}, false
	}
	atoi := func(v string) int {
		n, _ := strconv.Atoi(v) // the pattern admits digits only
		return n
	}
	r := Release{
		Year: atoi(m[1]), Month: atoi(m[2]), Patch: atoi(m[3]),
		Pre: m[4], Dirty: m[8] != "",
	}
	if r.Pre != "" {
		r.PreNum = atoi(m[5])
	}
	if m[6] != "" {
		n, err := strconv.Atoi(m[6])
		if err != nil {
			return Release{}, false // a commit count that overflows int is not a build
		}
		r.Commits, r.Describe = n, true
	}
	return r, true
}

// preRank orders pre-release kinds before the release they precede.
var preRank = map[string]int{"alpha": 0, "beta": 1, "rc": 2, "": 3}

// CompareReleases returns -1, 0 or +1 as a is older than, level with, or newer
// than b. Fields compare as integers. On an equal YYYY.M.P a pre-release sorts
// before the release (alpha < beta < rc, then by number), and a describe stamp
// sorts after its tag by commit count. The hash and -dirty carry no order:
// equal means the same position, not the same build.
func CompareReleases(a, b Release) int {
	for _, d := range [...][2]int{
		{a.Year, b.Year}, {a.Month, b.Month}, {a.Patch, b.Patch},
		{preRank[a.Pre], preRank[b.Pre]}, {a.PreNum, b.PreNum},
		{a.Commits, b.Commits},
	} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

// IsTaggedRelease reports whether s names a published release (or
// pre-release) as such: it parses, and carries no describe part and no -dirty.
// Such a string is shared by every install of that release, which is why
// telemetry may send it and must collapse anything else.
func IsTaggedRelease(s string) bool {
	r, ok := ParseRelease(s)
	return ok && !r.Describe && !r.Dirty
}
