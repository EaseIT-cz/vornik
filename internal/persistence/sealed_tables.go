package persistence

// SealedTables lists every table that holds sealed values (GitHub #571 U3a;
// mcp-server-authentication-design.md §15.2 A5; multi-tenancy design §7).
// The per-tenant key slice's rotation sweep and erasure step will iterate this
// list, never a hand-written query per table, so a sealed table missing from it
// would lose its data when an old key is destroyed. The architecture test
// (persistence/sqlite sealed_tables_571_test.go and its Postgres twin) fails,
// as a symmetric diff, when a table holds sealed values and is not listed, when
// a listed column holds none, or when a listed table lacks key_version.
var SealedTables = []SealedTable{
	{Table: "agent_secrets", Kind: SealedAEADColumns, Columns: []string{"ciphertext"}},
	{Table: "mcp_oauth_tokens", Kind: SealedPrefixed, Columns: []string{"access_token", "refresh_token"}},
	// A standing grant's key values (broker write-actions design, tier 2;
	// brokergrants.Store seals them). Missing from the multi-tenancy design's
	// list; found by the Postgres registry test the day it first ran.
	{Table: "broker_standing_grants", Kind: SealedPrefixed, Columns: []string{"key_values"}},
	// A tool-issued credential (credential carryover; GitHub #578), sealed by
	// executor.SealedTaskCredentials.
	{Table: "task_credentials", Kind: SealedPrefixed, Columns: []string{"value"}},
}

// SealedTable is one table of SealedTables.
type SealedTable struct {
	Table   string
	Kind    SealedKind
	Columns []string
}

// SealedKind is how a table's sealed values are stored, which is how the
// architecture test detects them.
type SealedKind int

const (
	// SealedPrefixed is a text column holding secretstore's "sv1:" values,
	// detected by scanning every text column for the prefix.
	SealedPrefixed SealedKind = iota
	// SealedAEADColumns is a ciphertext column beside a nonce column (the
	// secret store's own rows), detected by a column named ciphertext.
	SealedAEADColumns
)
