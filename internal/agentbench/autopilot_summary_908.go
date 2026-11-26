package agentbench

import "fmt"

// AutopilotSummary counts what the autopilot did in one arm, over every
// event in its autopilot.json (GitHub #908, benchmark design §12.38).
type AutopilotSummary struct {
	Triggers     int
	Generates    int // generate calls, whatever their outcome
	Candidates   int // generate calls that produced a candidate
	NoCandidate  int // the producer answered and made nothing
	Failed       int // the call got no answer, or a 5xx
	Trials       int
	TrialsPassed int
	Promoted     int
}

// SummarizeAutopilot counts the arm's events by step and outcome.
func SummarizeAutopilot(st AutopilotState) AutopilotSummary {
	var s AutopilotSummary
	for _, e := range st.Events {
		switch e.Step {
		case "trigger":
			s.Triggers++
		case "generate":
			s.Generates++
			switch {
			case e.Verdict == "failed":
				s.Failed++
			case e.Verdict == "no_candidate":
				s.NoCandidate++
			case e.CandidateID != "":
				s.Candidates++
			}
		case "trial":
			s.Trials++
			if e.Verdict == "trial_passed" {
				s.TrialsPassed++
			}
		case "promote":
			if e.Verdict == "promoted" {
				s.Promoted++
			}
		}
	}
	return s
}

// String is the summary the autopilot prints after every run. An arm in
// which no generate call produced a candidate says so in words, failures
// first: in the run that found #908, 12 of 12 calls failed and the arm read
// as "nothing to fix" while it had run as a baseline.
func (s AutopilotSummary) String() string {
	line := fmt.Sprintf("autopilot summary: %d triggers; %d generate calls: %d candidates, %d no candidate, %d failed; %d trials (%d passed); %d promoted",
		s.Triggers, s.Generates, s.Candidates, s.NoCandidate, s.Failed, s.Trials, s.TrialsPassed, s.Promoted)
	switch {
	case s.Generates == 0:
		return line + "\nautopilot: no generate call has run in this arm yet"
	case s.Candidates == 0:
		return line + fmt.Sprintf("\nautopilot: no generate call produced a candidate (%d of %d failed, %d no candidate): this arm learned nothing and ran as a baseline",
			s.Failed, s.Generates, s.NoCandidate)
	}
	return line
}
