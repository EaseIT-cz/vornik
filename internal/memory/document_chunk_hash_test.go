package memory

import "testing"

// A document ingest's chunk hash includes its upload (memory rollback x
// supersession design, A.7). Incident: project-wide dedup on sha256(text)
// stored only a changed document's changed chunks, so supersession retired
// the unchanged sections with the old upload (https://docs.vornik.io: 5 of 39).
func TestDocumentChunkHash(t *testing.T) {
	plain := ContentHash("section")
	a := DocumentChunkHash("artifact_a", "section")
	b := DocumentChunkHash("artifact_b", "section")
	if a == plain || b == plain || a == b {
		t.Fatalf("salted hashes must differ from the plain hash and from each other: %s %s %s", plain, a, b)
	}
	if a != DocumentChunkHash("artifact_a", "section") {
		t.Fatal("the salted hash must be deterministic")
	}
	// Empty text still hashes in the salted scheme (review R1).
	if DocumentChunkHash("artifact_a", "") == ContentHash("") {
		t.Fatal("an empty document chunk must not hash like an empty plain chunk")
	}
	// "a" + "bc" and "ab" + "c" must not collide: the separator matters.
	if DocumentChunkHash("a", "bc") == DocumentChunkHash("ab", "c") {
		t.Fatal("artifact and text must be separated")
	}
}

func TestIsDocumentChunkHash(t *testing.T) {
	salted := DocumentChunkHash("artifact_a", "old text")
	if !IsDocumentChunkHash(salted, "artifact_a", "old text") {
		t.Fatal("a salted hash must be detected as salted")
	}
	if IsDocumentChunkHash(ContentHash("old text"), "artifact_a", "old text") {
		t.Fatal("a plain hash must not be detected as salted")
	}
	if IsDocumentChunkHash(salted, "", "old text") {
		t.Fatal("no artifact means no salt")
	}
}
