package cli

import "fmt"

// reverifyAllowSelfCertified is --allow-self-certified.
var reverifyAllowSelfCertified bool

func init() {
	memoryReverifyDelegatedCmd.Flags().BoolVar(&reverifyAllowSelfCertified, "allow-self-certified", false,
		"Let --restore reinstate role-of-record stamps (the count is recorded on the restore event)")
}

// restoreReason is --restore's guard (round-3 review C4; design 22, part 8i):
// a restore that would reinstate role-of-record stamps (validator = producer;
// the hourly sweep never sees those) is refused unless --allow-self-certified
// is given, and then the event's recorded reason says how many it reinstated.
func restoreReason(selfCertified int, allow bool, reason string) (string, error) {
	if selfCertified == 0 {
		return reason, nil
	}
	if !allow {
		return "", fmt.Errorf("refusing --restore: it would reinstate %d role-of-record stamps that no sweep re-checks; "+
			"pass --allow-self-certified to restore them anyway (the count is recorded on the event)", selfCertified)
	}
	return fmt.Sprintf("%s [reinstates %d self-certified stamps; --allow-self-certified]", reason, selfCertified), nil
}

// noFetchApplyWarning is the --no-fetch --apply warning (round-3 C5; review
// 2811 F2): the tracking ref is trusted as it stands, and without a
// daemon-only memory.provenance_clone it lives in the project's workspace
// clone, which agents' worktrees share.
func noFetchApplyWarning(clone string) string {
	const base = "warning: --no-fetch --apply verifies against the clone's tracking ref as it stands; " +
		"a ref older than origin (or one written before the push guard) is trusted"
	if clone == "" {
		return base + "; this project sets no memory.provenance_clone, so that ref is in the workspace clone agents share"
	}
	return base
}
