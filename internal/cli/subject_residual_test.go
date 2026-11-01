package cli

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// #346 decision Q2 (art17 redact design §15.19). Found at: review, observed
// at integration (the stored erasure report): the residual's ground was a
// fixed sentence applied to every request, with no assessment of the request.
func TestResidualAssessment(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	shared := func() *datasubject.ErasurePlan {
		return &datasubject.ErasurePlan{Actions: []datasubject.Action{
			{Table: datasubject.TableChatAuditLog, RowID: "c1", Disposition: datasubject.DispositionRedact}}}
	}
	for name, in := range map[string][2]string{"no text": {"", "dpo"}, "no assessor": {"Art 17(3)(a) ...", " "}} {
		err := assessResidual(shared(), in[0], in[1], now)
		if err == nil || !strings.Contains(err.Error(), "--residual-assessment") || !strings.Contains(err.Error(), "--assessed-by") {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := shared()
	if err := assessResidual(p, "GDPR Art 17(3)(a): their own speech", "dpo", now); err != nil {
		t.Fatal(err)
	}
	if a := p.ResidualAssessment; a == nil || a.Text != "GDPR Art 17(3)(a): their own speech" || a.By != "dpo" || !a.At.Equal(now) {
		t.Fatalf("assessment = %+v", a)
	}
	// A plan that cannot leave a residual needs neither flag.
	none := &datasubject.ErasurePlan{Actions: []datasubject.Action{
		{Table: datasubject.TableChatAuditLog, RowID: "c2", Disposition: datasubject.DispositionDelete}}}
	if err := assessResidual(none, "", "", now); err != nil || none.ResidualAssessment != nil {
		t.Fatalf("no residual: %v %+v", err, none.ResidualAssessment)
	}
}
