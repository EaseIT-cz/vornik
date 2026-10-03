package agentadmin

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Review 20261003-6fec item 5 (broker design §18.7 F3): the count word comes
// from the number of documents, not a constant, so a raised per-workflow
// limit cannot make the approval sentence say "two" about three.
func TestDocumentPhrase_CountsTheDocuments(t *testing.T) {
	d := registry.BrokerDocument{Property: "a", MaxBytes: 1024, MediaType: "text/plain"}
	if got := documentPhrase([]registry.BrokerDocument{d, d}); !strings.HasPrefix(got, "two documents, of up to ") {
		t.Fatalf("two: %q", got)
	}
	if got := documentPhrase([]registry.BrokerDocument{d, d, d}); !strings.HasPrefix(got, "three documents, of up to ") {
		t.Fatalf("three: %q", got)
	}
	twelve := make([]registry.BrokerDocument, 12)
	for i := range twelve {
		twelve[i] = d
	}
	if got := documentPhrase(twelve); !strings.HasPrefix(got, "12 documents, of up to ") {
		t.Fatalf("twelve: %q", got)
	}
}
