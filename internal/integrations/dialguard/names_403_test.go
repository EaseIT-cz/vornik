package dialguard

// names is a list of plain entries as Go code builds them, unparsed: the
// grant canonicalises each one and skips one the loader would refuse
// (GitHub #403, §6.12).
func names(hosts ...string) []PrivateHost {
	out := make([]PrivateHost, len(hosts))
	for i, h := range hosts {
		out[i] = PrivateHost{Host: h}
	}
	return out
}
