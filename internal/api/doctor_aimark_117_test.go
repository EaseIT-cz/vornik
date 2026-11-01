package api

// GitHub #117: the doctor says whether Art 50(2) marking is on, and counts
// marks attached but not recorded (Art 50 design §14.7 F4, §14.8 B).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/persistence"
)

type failingMarkLedger struct{}

func (failingMarkLedger) Record(context.Context, persistence.AIOutputMark) error {
	return errors.New("db down")
}
func (failingMarkLedger) RecordKey(context.Context, persistence.AIMarkKey) error { return nil }

func TestDoctorAIMarking117(t *testing.T) {
	h := &DoctorHandlers{}
	if c := h.checkAIOutputMarking(); c.Status != "SKIPPED" || c.Name != "ai_output_marking" {
		t.Errorf("unwired: %+v", c)
	}
	h.SetAIMarker(nil)
	if c := h.checkAIOutputMarking(); c.Status != "WARNING" || !strings.Contains(c.Message, "off") {
		t.Errorf("no marker: %+v", c)
	}
	m, signer, _ := testMarker()
	h.SetAIMarker(m)
	if c := h.checkAIOutputMarking(); c.Status != "OK" || !strings.Contains(c.Message, signer.KID()) {
		t.Errorf("on: %+v", c)
	}
	bad := aimark.NewMarker(aimark.NewSigner(make([]byte, 32)), failingMarkLedger{}, func() string { return "t" }, nil, nil)
	_, _, _ = bad.New(context.Background(), []byte("x"), "s", "")
	h.SetAIMarker(bad)
	if c := h.checkAIOutputMarking(); c.Status != "WARNING" || !strings.Contains(c.Message, "1 mark") {
		t.Errorf("ledger errors: %+v", c)
	}
}
