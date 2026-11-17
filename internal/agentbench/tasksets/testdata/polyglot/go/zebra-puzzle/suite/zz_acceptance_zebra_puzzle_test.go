package zebra

import "testing"

func TestAcceptance_SolvePuzzle(t *testing.T) {
	expected := Solution{DrinksWater: "Norwegian", OwnsZebra: "Japanese"}
	actual := SolvePuzzle()
	if expected != actual {
		t.Fatalf("FAILED:\nExpected: %#v\nActual: %#v",
			expected, actual)
	}
}

func BenchmarkScore(b *testing.B) {
	for b.Loop() {
		SolvePuzzle()
	}
}
