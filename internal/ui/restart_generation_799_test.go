package ui

// GitHub #799 (operations-API design §6.13 G1): every restart-pending mark,
// a failed reload or a model-provider save, bumps the persisted restart
// generation; a bump that fails is logged and the mark stands.
//
// Found at: review (vornik-cloud control-plane design 5.5), observed at
// unit (the marks count nothing).

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

type bumper799 struct {
	n   int
	err error
}

func (b *bumper799) Bump() error { b.n++; return b.err }

func TestRestartGeneration799_EveryMarkBumps(t *testing.T) {
	b := &bumper799{}
	s := &Server{logger: zerolog.Nop()}
	WithRestartGeneration(b)(s)
	s.markRestartPending("a reload failed")
	s.markModelProviderPending()
	if b.n != 2 {
		t.Fatalf("bumps = %d, want 2", b.n)
	}
}

func TestRestartGeneration799_AFailedBumpKeepsTheMark(t *testing.T) {
	b := &bumper799{err: errors.New("disk full")}
	s := &Server{logger: zerolog.Nop()}
	WithRestartGeneration(b)(s)
	s.markRestartPending("a reload failed")
	if !s.restartBanner().Pending {
		t.Fatal("a failed bump dropped the mark")
	}
}

func TestRestartGeneration799_NoCounterIsFine(_ *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	s.markRestartPending("a reload failed")
	s.markModelProviderPending()
}
