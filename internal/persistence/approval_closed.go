package persistence

import (
	"unicode/utf8"
)

// ApprovalClosedPrefix starts the last_error of a task the cancel cascade
// moved out of AWAITING_APPROVAL (05-scheduler §4.11 "Amended 2026-11-07",
// GitHub #490). The inbox lists such tasks with LIKE '<prefix>%', so the
// prefix holds no LIKE wildcard.
const ApprovalClosedPrefix = "approval closed: "

// approvalClosedRootReasonMax bounds how much of the root's own reason a
// closed approval repeats.
const approvalClosedRootReasonMax = 300

// ApprovalClosedReason is the one builder of a closed approval's reason. root
// is the task CancelChildren was called for, read once; nil means it could not
// be read. A root that is not CANCELLED was retried: a retry cancels the
// superseded attempt's children too.
func ApprovalClosedReason(rootID string, root *Task) string {
	base := ApprovalClosedPrefix + "task " + rootID + " above it was "
	if root != nil && root.Status != TaskStatusCancelled {
		return base + "retried"
	}
	base += "cancelled"
	if root == nil || root.LastError == nil || *root.LastError == "" {
		return base
	}
	return base + ": " + cutAtRune(*root.LastError, approvalClosedRootReasonMax)
}

// cutAtRune returns at most limit bytes of s, never splitting a rune.
func cutAtRune(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
