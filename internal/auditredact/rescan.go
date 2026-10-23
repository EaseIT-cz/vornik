package auditredact

import "vornik.io/vornik/internal/secrets"

// The historical half of the transcript seams (chat-audit retention and
// redaction design §10.2.1, GitHub #414). `secrets scan-history` rescans rows
// written before a seam existed, and it must apply the rules the seam would
// apply to that row today. These entry points are that seam's own walk with a
// selection, so there is one implementation of each rule, not a second copy in
// internal/cli that would drift.

// Selector decides which finding types a rescan writes back. Nil selects
// everything, which is what the live seam does.
type Selector func(findingType string) bool

// Rescan is what one column's rescan found and what it redacted. Found counts
// every finding whether or not it was selected; Redacted only those written
// back. A column with Found empty did not match.
type Rescan struct {
	Found    map[string]int
	Redacted map[string]int
}

// Matched reports whether the column held anything any rule matched.
func (r Rescan) Matched() bool { return len(r.Found) > 0 }

func (s *transcriptScan) result() Rescan { return Rescan{Found: s.found, Redacted: s.counts} }

func newSelectedScan(det secrets.Detector, dropEntropy bool, sel Selector) *transcriptScan {
	s := newTranscriptScan(det, dropEntropy)
	s.sel = sel
	return s
}

// RescanIdentity applies the identity rule (§10.2.3) to a stored chat_id,
// user_id or author_id: the detector without the entropy class, each selected
// finding replaced in place.
func RescanIdentity(det secrets.Detector, sel Selector, value string) (string, Rescan) {
	s := newSelectedScan(det, true, sel)
	out := s.text(value)
	return out, s.result()
}

// TaskMessageRescan is one task_messages row after a rescan.
type TaskMessageRescan struct {
	Content  string
	AuthorID string
	Metadata []byte

	ContentScan, AuthorScan, MetadataScan Rescan
}

// RescanTaskMessage applies the TaskMessages seam's rules (§9.2.3, §10.2.3)
// to one stored row: content with the full detector; metadata decoded and
// walked without entropy, plus the key-name rule, re-encoded only when
// something changed; author_id with the identity rule.
func RescanTaskMessage(det secrets.Detector, sel Selector, content, authorID string, meta []byte) TaskMessageRescan {
	c := newSelectedScan(det, false, sel)
	m := newSelectedScan(det, true, sel)
	a := newSelectedScan(det, true, sel)
	out := TaskMessageRescan{
		Content:  c.text(content),
		Metadata: metadata(m, meta),
		AuthorID: a.text(authorID),
	}
	out.ContentScan, out.MetadataScan, out.AuthorScan = c.result(), m.result(), a.result()
	return out
}

// RescanChatSignals applies the ChatAudit seam's signal rules (§9.2.2) to a
// stored hallucination_signals_json. sources are the same row's free-text
// columns (user_message, response, tool_calls_json): their SELECTED findings
// are the fragment rule's known matches, exactly as the live seam scans those
// fields before the signals. The returned Rescan covers the signals column
// only; the caller counts the sources itself.
func RescanChatSignals(det secrets.Detector, sel Selector, sources []string, signalsJSON string) (string, Rescan) {
	src := newSelectedScan(det, false, sel)
	for _, text := range sources {
		src.text(text)
	}
	s := newSelectedScan(det, false, sel)
	s.known = src.known
	out := signals(s, signalsJSON)
	return out, s.result()
}
