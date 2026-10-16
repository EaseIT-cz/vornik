package registry

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// A `since:` tag must name a release that exists: one with a heading in the
// public release notes (docs/public/release-notes/index.md, which ships in
// both editions and carries the next release's draft). d051b21a4 (2026-10-13)
// corrected forge.issues keys tagged "2026.10.11" and "2026.10.12", versions
// no release has, with no test; the found-at-level retro of 2026-10-14
// (docs/audits/2026-10-14-found-at-level-retro.md) re-ran the same question
// over every tag and found five more: three fields shipped in 2026.10.1 tagged
// "2026.9.9" (never released) and two shipped in 2026.10.5 tagged "2026.10.7"
// (a future release). A wrong tag is wrong provenance in what an operator
// reads when upgrading (loader-validator agreement design §13.2, amendment
// 2026-10-14).
//
// The population is every struct tag in internal/ (non-test Go), not only the
// leaves ProjectConfigKeysSince walks: a tag on a struct-valued field
// (companion_push) is not a leaf and the walk never sees it.
func TestSinceTagsNameAPublishedRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "public", "release-notes", "index.md"))
	if err != nil {
		t.Fatalf("read the public release notes: %v", err)
	}
	releases := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^## (\d{4}\.\d{1,2}\.\d+)\s*$`).FindAllStringSubmatch(string(raw), -1) {
		releases[m[1]] = true
	}
	if len(releases) < 10 {
		t.Fatalf("found %d release headings in the public release notes; the parse is wrong", len(releases))
	}

	tagRe := regexp.MustCompile("`[^`\n]*\\bsince:\"([^\"]*)\"[^`\n]*`")
	var bad []string
	files, tags := 0, 0
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir("..", path, d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range tagRe.FindAllStringSubmatch(line, -1) {
				tags++
				if !releases[m[1]] {
					bad = append(bad, filepath.ToSlash(path)+":"+itoa(i+1)+` since:"`+m[1]+`"`)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	t.Logf("examined %d since tags in %d Go files under internal/ against %d published release headings", tags, files, len(releases))
	if tags == 0 {
		t.Fatal("examined no since tags: the scan is wrong")
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("%d since tag(s) name no release in docs/public/release-notes/index.md; tag the release that shipped the field:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
