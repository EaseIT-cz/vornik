package agentbench

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Run kinds and arm binding — release-gate design §9 (2026-09-23).
//
// Incident: the hard-tier calibration of 2026-09-20 ran `--arm
// 2026.9.5-hard-cal` under a pre-registration declaring ["2026.9.4-rc",
// "2026.9.5-d6"], because a single-arm pass could not be pre-registered at all
// and nothing compared --arm to the declared arms. The journal carried a hash
// committing to a comparison it was not part of (https://docs.vornik.io P2 2026-09-20).

func calibrationPreReg() PreRegistration {
	return PreRegistration{
		Kind:      RunKindCalibration,
		Arms:      []string{"2026.9.5-hard-cal"},
		Metric:    PinnedCaseValidationMetric,
		Rationale: "measure per-task pass rates of the hard tier",
	}
}

func TestPreRegistration_MeasurementKindsAcceptOneArmAndNoSigma(t *testing.T) {
	for _, kind := range []RunKind{RunKindCalibration, RunKindNoiseFloor} {
		p := calibrationPreReg()
		p.Kind = kind
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: a single-arm measurement pass was refused: %v", kind, err)
		}
	}
}

func TestPreRegistration_MeasurementKindsRefuseWhatTheyCannotHonestlyDeclare(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PreRegistration)
		want string
	}{
		{"two arms", func(p *PreRegistration) { p.Arms = []string{"a", "b"} }, "exactly one arm"},
		{"no arm", func(p *PreRegistration) { p.Arms = nil }, "exactly one arm"},
		{"an empty arm", func(p *PreRegistration) { p.Arms = []string{" "} }, "empty arm"},
		{"a declared sigma", func(p *PreRegistration) { p.SigmaD, p.SigmaN = 0.02, 10 }, "sigma"},
		{"a target delta", func(p *PreRegistration) { p.TargetDelta = 0.05 }, "target delta"},
		{"computed pairs", func(p *PreRegistration) { p.ComputedPairs = 13 }, "computed pair"},
		{"a calibration hash", func(p *PreRegistration) {
			p.CalibrationSHA256 = strings.Repeat("a", 64)
		}, "release"},
		{"a noise-floor hash", func(p *PreRegistration) {
			p.NoiseFloorSHA256 = strings.Repeat("b", 64)
		}, "release"},
		{"a gate-policy hash", func(p *PreRegistration) {
			p.ReleaseGatePolicySHA256 = strings.Repeat("c", 64)
		}, "release"},
		{"independent axes", func(p *PreRegistration) {
			p.IndependentAxes = []string{"binary_sha256"}
		}, "independent axes"},
		{"no metric", func(p *PreRegistration) { p.Metric = "" }, "no metric"},
		{"no rationale", func(p *PreRegistration) { p.Rationale = "" }, "no rationale"},
	}
	for _, kind := range []RunKind{RunKindCalibration, RunKindNoiseFloor} {
		for _, c := range cases {
			t.Run(string(kind)+"/"+c.name, func(t *testing.T) {
				p := calibrationPreReg()
				p.Kind = kind
				c.mut(&p)
				err := p.Validate()
				if err == nil {
					t.Fatalf("accepted a %s pre-registration with %s", kind, c.name)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("refusal does not name the problem (%q): %v", c.want, err)
				}
			})
		}
	}
}

func TestPreRegistration_UnknownKindIsRefusedNotDefaulted(t *testing.T) {
	p := validPreReg()
	p.Kind = "calibrate"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "unknown run kind") {
		t.Fatalf("an unknown kind was read as the default: %v", err)
	}
}

func TestPreRegistration_ExplicitComparisonKeepsTheTwoArmRule(t *testing.T) {
	p := validPreReg()
	p.Kind = RunKindComparison
	if err := p.Validate(); err != nil {
		t.Fatalf("explicit comparison refused: %v", err)
	}
	p.Arms = []string{"only"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "two arms") {
		t.Fatalf("explicit comparison accepted one arm: %v", err)
	}
}

// Every committed pre-registration predates the field. Its bytes, and so the
// hash a published journal carries, must not move.
func TestPreRegistration_AbsentKindKeepsCommittedHashes(t *testing.T) {
	blob, err := json.Marshal(validPreReg())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "kind") {
		t.Fatalf("an absent kind is serialised, which moves every committed hash: %s", blob)
	}
	if validPreReg().EffectiveKind() != RunKindComparison {
		t.Fatalf("absent kind = %q, want comparison", validPreReg().EffectiveKind())
	}
}

func TestPreRegistration_BindsTheArmBeingRun(t *testing.T) {
	cmp := validPreReg() // arms: baseline, suppressed
	if err := cmp.BindArm("baseline"); err != nil {
		t.Fatalf("a declared arm was refused: %v", err)
	}
	// The incident shape: a run under a pre-registration for other arms.
	err := cmp.BindArm("2026.9.5-hard-cal")
	if err == nil {
		t.Fatal("an undeclared arm ran under the pre-registration")
	}
	// §9.2's message: the supplied arm, EVERY declared arm, and that the
	// binding applies to comparisons — the first operator to meet this will be
	// running a legitimate comparison batch.
	for _, want := range []string{"2026.9.5-hard-cal", "baseline", "suppressed", "comparison"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if err := cmp.BindArm(""); err == nil || !strings.Contains(err.Error(), "--arm") {
		t.Fatalf("an unnamed arm ran under the pre-registration: %v", err)
	}
	cal := calibrationPreReg()
	if err := cal.BindArm("2026.9.5-hard-cal"); err != nil {
		t.Fatalf("calibration arm refused: %v", err)
	}
}

func measurementJournal(kind RunKind) Journal {
	j := readableJournal()
	pre := calibrationPreReg()
	pre.Kind = kind
	j.Manifest.PreRegistration = pre
	j.Manifest.PreRegistrationHash, _ = pre.Hash()
	j.Manifest.Power = PowerCheck{}
	return j
}

func TestCompareJournals_RefusesAMeasurementPass(t *testing.T) {
	for _, kind := range []RunKind{RunKindCalibration, RunKindNoiseFloor} {
		if _, err := CompareJournals(measurementJournal(kind), readableJournal(), 0.1); err == nil ||
			!strings.Contains(err.Error(), string(kind)) {
			t.Errorf("%s journal compared as the first side: %v", kind, err)
		}
		if _, err := CompareJournals(readableJournal(), measurementJournal(kind), 0.1); err == nil ||
			!strings.Contains(err.Error(), string(kind)) {
			t.Errorf("%s journal compared as the second side: %v", kind, err)
		}
	}
}

// A calibration pass is not sized, so "underpowered" is not a finding about it.
func TestCheckReadable_DoesNotCallAMeasurementPassUnderpowered(t *testing.T) {
	if err := measurementJournal(RunKindCalibration).CheckReadable(); err != nil {
		t.Fatalf("a correct calibration journal reads as degraded: %v", err)
	}
}

func TestGate_RefusesAMeasurementJournal(t *testing.T) {
	for _, kind := range []RunKind{RunKindCalibration, RunKindNoiseFloor} {
		err := validateReleaseJournal("baseline", measurementJournal(kind))
		if err == nil || !strings.Contains(err.Error(), "release gate artifacts") {
			t.Errorf("a %s journal was admitted to the release gate: %v", kind, err)
		}
	}
}

func TestBuildCalibration_RequiresACalibrationPass(t *testing.T) {
	for name, mut := range map[string]func(*Journal){
		"no pre-registration kind (legacy or borrowed)": func(j *Journal) {
			j.Manifest.PreRegistration = validPreReg()
			j.Manifest.PreRegistrationHash, _ = validPreReg().Hash()
		},
		"a noise-floor pass": func(j *Journal) {
			j.Manifest.PreRegistration.Kind = RunKindNoiseFloor
			j.Manifest.PreRegistrationHash, _ = j.Manifest.PreRegistration.Hash()
		},
		"a pre-registration edited after the run": func(j *Journal) {
			j.Manifest.PreRegistration.Rationale = "rewritten afterwards"
		},
	} {
		j := calibrationJournal()
		mut(&j)
		if _, err := BuildCalibration(j, releaseTestSHA("a")); err == nil ||
			!strings.Contains(err.Error(), "calibration") || !strings.Contains(err.Error(), "pre-registration") {
			t.Errorf("%s: calibration built from it: %v", name, err)
		}
	}
}

func TestBuildNoiseFloor_RequiresTwoNoiseFloorPasses(t *testing.T) {
	good := noiseFloorJournal(10, .4, .6)
	borrowed := noiseFloorJournal(10, .5, .4)
	borrowed.Manifest.PreRegistration = validPreReg()
	borrowed.Manifest.PreRegistrationHash, _ = validPreReg().Hash()
	if _, err := BuildNoiseFloor(good, borrowed, releaseTestSHA("a"), releaseTestSHA("b")); err == nil ||
		!strings.Contains(err.Error(), "pre-registration") {
		t.Fatalf("noise floor built from a comparison journal: %v", err)
	}
	if _, err := BuildNoiseFloor(borrowed, good, releaseTestSHA("a"), releaseTestSHA("b")); err == nil ||
		!strings.Contains(err.Error(), "pre-registration") {
		t.Fatalf("noise floor built from a comparison journal (first side): %v", err)
	}
	// Tamper detection, symmetric with calibrate: still kind noise_floor, but
	// the pre-registration was edited after the run and its hash is stale.
	edited := noiseFloorJournal(10, .5, .4)
	edited.Manifest.PreRegistration.Rationale = "rewritten afterwards"
	for _, pair := range [][2]Journal{{good, edited}, {edited, good}} {
		if _, err := BuildNoiseFloor(pair[0], pair[1], releaseTestSHA("a"), releaseTestSHA("b")); err == nil ||
			!strings.Contains(err.Error(), "edited after") {
			t.Errorf("noise floor built from a pass edited after the run: %v", err)
		}
	}
	// And the positive path still builds with two correctly-declared passes.
	if _, err := BuildNoiseFloor(good, noiseFloorJournal(10, .5, .4), releaseTestSHA("a"), releaseTestSHA("b")); err != nil {
		t.Fatalf("two noise_floor passes refused: %v", err)
	}
}

// §9.3a: no path that writes a journal constructs its pre-registration. A
// producer that rebuilt the struct and forgot Kind would turn a measurement
// journal into a comparison-eligible one, silently — omitempty makes the
// forgotten field indistinguishable from "comparison" (review ad48 F1).
func TestProducers_CarryTheKindTheyWereGiven(t *testing.T) {
	pre := calibrationPreReg()
	preHash, _ := pre.Hash()
	withKind := func(j *Journal) {
		j.Manifest.PreRegistration, j.Manifest.PreRegistrationHash = pre, preHash
	}

	merged, err := MergeJournals(jrnl("a", withKind), jrnl("b", withKind))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.Manifest.PreRegistration.Kind != RunKindCalibration {
		t.Errorf("merge dropped the kind: %q", merged.Manifest.PreRegistration.Kind)
	}

	in := v2Journal()
	withKind(&in)
	traces := rescoreTraces{byExec: map[string][]Trace{"e1": {{
		ExecutionID: "e1", StepID: "s1", Role: "lead",
		Requested: []string{"run_shell"}, Accepted: []string{"run_shell"}, Invoked: []string{"run_shell"},
	}}}}
	out, err := Rescore(context.Background(), in, traces, []Probe{GrantProbe{}}, goldFor("run_shell"))
	if err != nil {
		t.Fatalf("rescore: %v", err)
	}
	if out.Manifest.PreRegistration.Kind != RunKindCalibration || out.Manifest.PreRegistrationHash != preHash {
		t.Errorf("rescore did not carry the pre-registration: kind=%q", out.Manifest.PreRegistration.Kind)
	}
}

// §9.3a, the third producer: Runner.Run journals the struct it was given.
// Also §9.2's defence in depth — the library refuses an unbound arm even if a
// caller other than the CLI forgets to.
func TestRunner_JournalsTheKindAndRefusesAnUnboundArm(t *testing.T) {
	cfg := validConfig()
	cfg.PreRegistration = calibrationPreReg()
	cfg.PreRegistration.Arms = []string{cfg.Arm.Name}
	cfg.Power = PowerCheck{}
	j, err := (&Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}}).Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("calibration run: %v", err)
	}
	wantHash, _ := cfg.PreRegistration.Hash()
	if j.Manifest.PreRegistration.Kind != RunKindCalibration || j.Manifest.PreRegistrationHash != wantHash {
		t.Errorf("Runner.Run did not journal the pre-registration it was given: kind=%q", j.Manifest.PreRegistration.Kind)
	}

	tasks := &fakeTasks{}
	unbound := validConfig()
	unbound.Arm.Name = "2026.9.5-hard-cal"
	if _, err := (&Runner{Tasks: tasks, Traces: &fakeTraces{}}).Run(context.Background(), unbound); err == nil {
		t.Fatal("Runner.Run ran an arm its pre-registration does not declare")
	}
	if len(tasks.calls) != 0 {
		t.Errorf("submitted %d task(s) for an unbound arm", len(tasks.calls))
	}
}

// The compare boundary reads journals without validating them, so it refuses an
// unknown kind itself rather than defaulting it to comparison (review 431c).
func TestCompareJournals_RefusesAnUnknownKind(t *testing.T) {
	odd := readableJournal()
	odd.Manifest.PreRegistration.Kind = "calibrate"
	if _, err := CompareJournals(odd, readableJournal(), 0.1); err == nil ||
		!strings.Contains(err.Error(), "unknown run kind") {
		t.Fatalf("an unknown-kind journal was compared as a comparison: %v", err)
	}
}
