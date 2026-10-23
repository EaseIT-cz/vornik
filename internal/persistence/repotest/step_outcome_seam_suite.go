package repotest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// RunStepOutcomeSeamSuite pins the write-time secret seam on
// execution_step_outcomes.hallucination_signals on a real backend (chat-audit
// retention and redaction design §11.2.1, GitHub #425).
//
// GitHub #425: the column had no write-time scan. A signal quotes the step's
// result after the result_json sink, which leaves a credential intact under
// `detect` or with scanning off, and the 256/240-byte cuts can split one at
// the edge. Found at: review (#414's §10.2.5). A user observes it at
// integration: the stored row holds the credential. So this suite records
// through the decorator into the real repository and reads the stored row
// back, on SQLite in the default lane and on Postgres (jsonb) under
// -tags=integration.
//
// decorate builds the seam over inner with det; it is a parameter because
// auditredact's own tests import this package (an import here would cycle).
func RunStepOutcomeSeamSuite(t *testing.T, inner persistence.ExecutionStepOutcomeRepository,
	decorate func(persistence.ExecutionStepOutcomeRepository, secrets.Detector) persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rig := stepOutcomeSeamRig{inner: inner, repo: decorate(inner, det)}
	t.Run("DirtySignalsAreRedacted", rig.dirtySignalsAreRedacted)
	t.Run("CleanSignalsAreByteIdentical", rig.cleanSignalsAreByteIdentical)
	t.Run("NoSignalsStaysNull", rig.noSignalsStaysNull)
	t.Run("StuckRecorderPassesThrough", rig.stuckRecorderPassesThrough)
}

// Synthetic credentials of the right shape; never real.
const (
	seam425GH  = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	seam425AWS = "AKIAQ3EGUZJ7RLW4XN2P"
)

type stepOutcomeSeamRig struct {
	inner, repo persistence.ExecutionStepOutcomeRepository
}

// record writes signals through the decorator and reads the stored row back
// from the undecorated repository.
func (r stepOutcomeSeamRig) record(t *testing.T, signals []byte) *persistence.ExecutionStepOutcome {
	t.Helper()
	ctx := context.Background()
	exec := uniqueID("exec425")
	o := &persistence.ExecutionStepOutcome{
		ID: uniqueID("out425"), ProjectID: "p425", TaskID: uniqueID("task425"), ExecutionID: exec,
		StepID: "write", Role: "writer", Model: "m", Outcome: "pending_validation",
		HallucinationSignals: signals,
	}
	passed := append([]byte(nil), signals...)
	if err := r.repo.Record(ctx, o); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if string(o.HallucinationSignals) != string(passed) {
		t.Fatal("the decorator must not edit the caller's struct")
	}
	rows, err := r.inner.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &exec, PageSize: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: %v (%d rows)", err, len(rows))
	}
	return rows[0]
}

func requireNoFragment425(t *testing.T, blob string, keys ...string) {
	t.Helper()
	for _, secret := range keys {
		for i := 0; i+10 <= len(secret); i++ {
			if strings.Contains(blob, secret[i:i+10]) {
				t.Fatalf("stored signals still hold a fragment of a key (%q): %s", secret[i:i+10], blob)
			}
		}
	}
}

func (r stepOutcomeSeamRig) dirtySignalsAreRedacted(t *testing.T) {
	// A key in claim_value, a key's head cut at the "…" edge of a sentence,
	// and a second key inside a sentence. The cut-edge one is the shape the
	// 240-byte cut makes of a credential the sink did not redact.
	dirty := `[{"detector":"url_not_fetched","severity":"warn","claim_type":"url",` +
		`"claim_value":"https://x.test/?token=` + seam425GH + `","sentence":"see ` + seam425AWS + ` here"},` +
		`{"detector":"path_not_found","severity":"info","claim_type":"path","claim_value":"/tmp/a",` +
		`"sentence":"the deploy used ` + seam425GH[:24] + `…","evidence_searched":"tool_audit (4 entries)"}]`
	stored := r.record(t, []byte(dirty))
	blob := string(stored.HallucinationSignals)
	requireNoFragment425(t, blob, seam425GH, seam425AWS)
	var sigs []map[string]any
	if err := json.Unmarshal(stored.HallucinationSignals, &sigs); err != nil || len(sigs) != 2 {
		t.Fatalf("signals must stay a two-element JSON array: %v %s", err, blob)
	}
	if sigs[0]["detector"] != "url_not_fetched" || sigs[1]["claim_value"] != "/tmp/a" ||
		sigs[1]["evidence_searched"] != "tool_audit (4 entries)" {
		t.Errorf("the signals' other fields must survive: %v", sigs)
	}
	if !strings.Contains(blob, "[REDACTED:") {
		t.Errorf("a redaction leaves a typed marker: %s", blob)
	}
}

func (r stepOutcomeSeamRig) cleanSignalsAreByteIdentical(t *testing.T) {
	clean := `[{"detector":"url_not_fetched","severity":"warn","claim_type":"url","claim_value":"https://example.test/a","sentence":"plain words"}]`
	stored := r.record(t, []byte(clean))
	var got, want any
	_ = json.Unmarshal(stored.HallucinationSignals, &got)
	_ = json.Unmarshal([]byte(clean), &want)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("clean signals changed: %s", stored.HallucinationSignals)
	}
}

func (r stepOutcomeSeamRig) noSignalsStaysNull(t *testing.T) {
	if stored := r.record(t, nil); len(stored.HallucinationSignals) != 0 {
		t.Fatalf("a row without signals must store none: %q", stored.HallucinationSignals)
	}
}

func (r stepOutcomeSeamRig) stuckRecorderPassesThrough(t *testing.T) {
	o := &persistence.ExecutionStepOutcome{
		ID: uniqueID("out425s"), ProjectID: "p425", TaskID: uniqueID("task425"), ExecutionID: uniqueID("exec425"),
		StepID: "write", Role: "writer", Model: "m", Outcome: "timeout", RecordedAt: time.Now().UTC(),
	}
	stuck, isStuck := r.repo.(interface {
		RecordStuckStepOutcome(ctx context.Context, o *persistence.ExecutionStepOutcome, since time.Time) (bool, error)
	})
	if !isStuck {
		t.Fatalf("the decorator hid RecordStuckStepOutcome: %T", r.repo)
	}
	ok, err := stuck.RecordStuckStepOutcome(context.Background(), o, time.Now().Add(-time.Hour))
	if err != nil || !ok {
		t.Fatalf("RecordStuckStepOutcome through the decorator: %v %v", ok, err)
	}
}
