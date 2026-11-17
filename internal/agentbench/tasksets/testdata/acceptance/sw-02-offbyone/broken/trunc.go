// Package trunc is a deliberately BROKEN variant (the classic off-by-one) for sw-02-offbyone's acceptance suite.
package trunc

func Truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	if max < 3 {
		return s[:max]
	}
	return s[:max-2] + "..."
}
