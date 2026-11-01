package persistence

import (
	"context"
	"time"
)

// AIOutputMark is one row of `ai_output_marks`: the record that the content
// with this SHA-256 was emitted with a signed AI mark (EU AI Act Art 50(2);
// GitHub #117; Art 50 design §14.7). The first write for a hash wins: the
// ledger proves the provenance of content, not an audit of tasks.
type AIOutputMark struct {
	ContentSHA256 string
	Surface       string
	// Mark is the compact signed mark.
	Mark string
	// TaskID is evidence only and never served by the public endpoint.
	TaskID   string
	IssuedAt time.Time
}

// AIMarkKey is one row of `ai_mark_keys`: a public key that has signed marks.
// Persisted so past marks stay verifiable when the key that derived it is
// gone (review 3701 F2).
type AIMarkKey struct {
	KID         string
	PublicKey   []byte
	FirstUsedAt time.Time
}

// AIOutputMarkRepository persists the Art 50(2) evidence: marks and the keys
// that signed them. Both tables are evidence: never pruned, on every backend.
type AIOutputMarkRepository interface {
	// Record writes a mark; a repeat for the same hash is a no-op.
	Record(ctx context.Context, m AIOutputMark) error
	// Get returns the mark for a content hash, or ErrNotFound.
	Get(ctx context.Context, contentSHA256 string) (*AIOutputMark, error)
	// RecordKey writes a key; a repeat for the same kid is a no-op.
	RecordKey(ctx context.Context, k AIMarkKey) error
	// Keys returns every key that has signed marks, oldest first.
	Keys(ctx context.Context) ([]AIMarkKey, error)
}
