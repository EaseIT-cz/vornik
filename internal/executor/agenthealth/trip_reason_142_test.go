package agenthealth

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
)

// audit 2026-10-12: GitHub #142 §S3 #16 (outcome-quality LLD §S5 #16). The
// agent breaker counted its trip reason in a metric and nowhere else: a trip
// was logged nowhere, and the doctor's snapshot could not say why a circuit
// was open.
func TestAgentBreakerTrip_IsLoggedWithReasonAndInTheSnapshot(t *testing.T) {
	var buf bytes.Buffer
	r := newReg(agentCfg(), true)
	r.SetLogger(zerolog.New(&buf))
	for i := 0; i < 3; i++ {
		r.Record("m", false, fmt.Errorf("agent reported FAILED status: PROVIDER_ERROR: upstream provider returned an error"))
	}
	log := buf.String()
	for _, want := range []string{`"model":"m"`, `"trip_reason":"rate"`, chat.TripReasonRemedy(chat.TripReasonRate)} {
		if !strings.Contains(log, want) {
			t.Errorf("trip log %q lacks %q", log, want)
		}
	}
	snaps := r.ModelHealthSnapshot()
	if len(snaps) != 1 || snaps[0].TripReason != chat.TripReasonRate {
		t.Errorf("snapshot = %+v, want TripReason rate for the doctor line", snaps)
	}
}
