package persistence

import "sort"

// UnionSuspendedTools is the suspended set after adding tools: sorted,
// without duplicates or empty names. Both drivers' SuspendIntegrationTools
// use it, so the stored order cannot differ by driver (design §6.5,
// amendment #481).
func UnionSuspendedTools(have, add []string) []string {
	seen := make(map[string]bool, len(have)+len(add))
	out := make([]string, 0, len(have)+len(add))
	for _, xs := range [][]string{have, add} {
		for _, t := range xs {
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
