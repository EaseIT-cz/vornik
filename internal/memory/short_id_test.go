package memory

import "testing"

// audit 2026-10-12: memory_list's short_id was the id's first 12 characters,
// so every correction chunk (chunk_YYYYMMDDhhmmss_<hex>) written in one month
// had the same short id ("chunk_202610") and Hermes could not tell them apart
// (design 22, "Short ids for every id shape").
func TestShortID_UniqueAndStableForEveryIDShape(t *testing.T) {
	hex := "0123456789abcdef0123456789abcdef"
	if got := ShortID(hex); got != "0123456789ab" {
		t.Fatalf("hex id: got %q, want its first 12 characters", got)
	}
	a := "chunk_20261012101500_00112233aabbccdd"
	b := "chunk_20261012101500_44556677eeff0011"
	sa, sb := ShortID(a), ShortID(b)
	if sa == sb {
		t.Fatalf("two correction ids from the same second share short id %q", sa)
	}
	for _, s := range []string{sa, sb, ShortID("abc"), ShortID("0123456789ABCDEF")} {
		if len(s) != shortIDLen || !isLowerHex(s) {
			t.Fatalf("short id %q is not %d lowercase hex characters", s, shortIDLen)
		}
	}
	if ShortID(a) != sa {
		t.Fatal("short id is not stable")
	}
	// Uppercase hex is not the content-hash shape: it is hashed.
	if got := ShortID("0123456789ABCDEF"); got == "0123456789AB" {
		t.Fatalf("uppercase id kept its prefix %q", got)
	}
	if got := ShortID(""); got != "" {
		t.Fatalf("empty id: got %q", got)
	}
}
