package dialguard

import (
	"fmt"
	"io"
)

// ReadBoundedBody reads at most limit bytes of r and refuses a longer body
// instead of handing back a truncated prefix (GitHub #433, moved here and
// exported under #465 into this leaf package, integrations-hub-design §0.0b). It is the one bounded
// read for the guarded fetches of configured URLs: the trading panel's /caps
// and the daemon's broker fetches.
func ReadBoundedBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
}
