package retention

import "testing"

// GitHub #117 (Art 50 design §14.7, M6): the Art 50(2) marking ledger and
// the keys that signed it are conformity evidence and never prunable.
func TestAIMarkTablesAreEvidence_117(t *testing.T) {
	for _, table := range []string{"ai_output_marks", "ai_mark_keys"} {
		if !evidenceTables[table] {
			t.Errorf("%s must be a retention evidence table", table)
		}
	}
}
