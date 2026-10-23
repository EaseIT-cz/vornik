package version

import "testing"

// Issue #371: releases were compared as strings, so 2026.10.x sorted before
// 2026.9.x. These pin the one grammar and the one order (loader-validator
// agreement design, 2026-10-17 amendment).

func TestParseReleaseAccepts(t *testing.T) {
	cases := []struct {
		in            string
		y, m, p       int
		pre           string
		preN, commits int
		dirty, tagged bool
	}{
		{"2026.9.4", 2026, 9, 4, "", 0, 0, false, true},
		{"2026.10.6", 2026, 10, 6, "", 0, 0, false, true},
		{"2026.09.01", 2026, 9, 1, "", 0, 0, false, true},
		{"2026.10.7-rc1", 2026, 10, 7, "rc", 1, 0, false, true},
		{"2026.10.7-rc.1", 2026, 10, 7, "rc", 1, 0, false, true},
		{"2026.10.7-alpha2", 2026, 10, 7, "alpha", 2, 0, false, true},
		{"2026.10.7-beta.10", 2026, 10, 7, "beta", 10, 0, false, true},
		{"2026.10.6-115-g44e675c12", 2026, 10, 6, "", 0, 115, false, false},
		{"2026.10.6-115-g44e675c12-dirty", 2026, 10, 6, "", 0, 115, true, false},
		{"2026.10.6-dirty", 2026, 10, 6, "", 0, 0, true, false},
		{"2026.10.7-rc1-3-gabc1234", 2026, 10, 7, "rc", 1, 3, false, false},
		{" 2026.10.6 ", 2026, 10, 6, "", 0, 0, false, true},
	}
	for _, c := range cases {
		r, ok := ParseRelease(c.in)
		if !ok {
			t.Errorf("ParseRelease(%q) rejected", c.in)
			continue
		}
		if r.Year != c.y || r.Month != c.m || r.Patch != c.p || r.Pre != c.pre || r.PreNum != c.preN || r.Commits != c.commits || r.Dirty != c.dirty {
			t.Errorf("ParseRelease(%q) = %+v", c.in, r)
		}
		if got := IsTaggedRelease(c.in); got != c.tagged {
			t.Errorf("IsTaggedRelease(%q) = %v, want %v", c.in, got, c.tagged)
		}
	}
}

func TestParseReleaseRejects(t *testing.T) {
	for _, in := range []string{
		"", "dev", "unstamped", "dev+g44e675c12", "dev+g44e675c12.dirty",
		"44e675c12", "2026.10", "2026", "v2026.10.6", "2026.10.6+meta",
		"2026.10.6-dev", "2026.10.7-rc", "2026.10.7-rc1234", "2026.10.7-hotfix1",
		"26.10.6", "2026.100.6", "2026.10.1000", "2026.10.6-115", "2026.10.6-115-gXYZ",
		"2026.10.6-dirty-dirty", "2026.10.6.1", "2026..6",
	} {
		if r, ok := ParseRelease(in); ok {
			t.Errorf("ParseRelease(%q) accepted: %+v", in, r)
		}
		if IsTaggedRelease(in) {
			t.Errorf("IsTaggedRelease(%q) = true", in)
		}
	}
}

func mustParse(t *testing.T, s string) Release {
	t.Helper()
	r, ok := ParseRelease(s)
	if !ok {
		t.Fatalf("ParseRelease(%q) rejected", s)
	}
	return r
}

// Each entry is strictly older than the next.
func TestCompareReleasesOrder(t *testing.T) {
	chain := []string{
		"2025.12.9",
		"2026.1.0",
		"2026.9.4",
		"2026.9.8",
		"2026.10.0", // the month boundary string comparison got wrong
		"2026.10.6",
		"2026.10.6-3-gaaaa111",
		"2026.10.6-115-g44e675c12",
		"2026.10.7-alpha1",
		"2026.10.7-alpha.2",
		"2026.10.7-beta1",
		"2026.10.7-rc1",
		"2026.10.7-rc2",
		"2026.10.7-rc10",
		"2026.10.7-rc10-4-gbbbb222",
		"2026.10.7",
		"2026.10.7-1-gcccc333",
		"2026.10.7-2-gdddd444",
		"2026.10.10",
		"2026.11.0",
		"2027.1.0",
	}
	for i := range chain {
		for j := range chain {
			a, b := mustParse(t, chain[i]), mustParse(t, chain[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := CompareReleases(a, b); got != want {
				t.Errorf("CompareReleases(%s, %s) = %d, want %d", chain[i], chain[j], got, want)
			}
		}
	}
}

// Hash, dirtiness, the pre-release dot and leading zeros carry no order.
func TestCompareReleasesEqual(t *testing.T) {
	for _, p := range [][2]string{
		{"2026.10.6-115-g44e675c12", "2026.10.6-115-gffffffff-dirty"},
		{"2026.10.6", "2026.10.6-dirty"},
		{"2026.10.7-rc1", "2026.10.7-rc.1"},
		{"2026.09.01", "2026.9.1"},
	} {
		if got := CompareReleases(mustParse(t, p[0]), mustParse(t, p[1])); got != 0 {
			t.Errorf("CompareReleases(%s, %s) = %d, want 0", p[0], p[1], got)
		}
	}
}
