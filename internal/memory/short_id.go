package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// shortIDLen is the length of a memory item's short id.
const shortIDLen = 12

// ShortID is the display handle of a memory chunk id, shown by memory_list
// and the approver device's memory page (design 22, "Short ids for every id
// shape", audit 2026-10-12). It is a function of the id alone, so it is
// stable, and it is unique for both id shapes the store holds:
//
//   - a content-hash id (12 or more lowercase hex characters, memory.chunkID):
//     its first 12 characters, the short id users have always seen;
//   - any other id, such as a correction chunk's
//     chunk_YYYYMMDDhhmmss_<16 hex> (GenerateChunkID), whose prefix is the
//     same for every row written in one month: the first 12 hex characters
//     of its sha256.
//
// A short id is never what memory_correct takes; clients map it back to the
// full id through the listing they showed. Clients that compute it locally
// (the Hermes plugin) implement the same rule.
func ShortID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) >= shortIDLen && isLowerHex(id) {
		return id[:shortIDLen]
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:shortIDLen]
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
