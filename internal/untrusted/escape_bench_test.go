package untrusted

import (
	"strings"
	"testing"
)

// Wrap runs on every MCP result, artifact read and memory hit, some of them
// megabytes long. These benchmarks keep the escape's cost visible.
func BenchmarkWrapASCII1MiB(b *testing.B) {
	s := strings.Repeat("plain ascii text with a < b comparison\n", 1<<20/40)
	b.SetBytes(int64(len(s)))
	for i := 0; i < b.N; i++ {
		_ = Wrap(s)
	}
}

// Worst case for the name match: every byte is a candidate start followed
// by separators only, so each start reads the full bounded view.
func BenchmarkWrapPathological1MiB(b *testing.B) {
	s := strings.Repeat("< ", 1<<19)
	b.SetBytes(int64(len(s)))
	for i := 0; i < b.N; i++ {
		_ = Wrap(s)
	}
}

// Every byte starts an escape the layered decoder tries to peel.
func BenchmarkWrapAmpRun1MiB(b *testing.B) {
	s := strings.Repeat("&amp;", 1<<20/5)
	b.SetBytes(int64(len(s)))
	for i := 0; i < b.N; i++ {
		_ = Wrap(s)
	}
}

func BenchmarkWrapNonASCII1MiB(b *testing.B) {
	s := strings.Repeat("Příliš žluťoučký kůň úpěl ďábelské ódy\n", 1<<20/48)
	b.SetBytes(int64(len(s)))
	for i := 0; i < b.N; i++ {
		_ = Wrap(s)
	}
}
