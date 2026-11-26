package api

// healing_judge_family (GitHub #891, config-assistant design §7.3a). Workflow
// healing calls the config assistant as its candidate producer without the
// feature doctor's family check, so an install whose assistant and judge
// share a family (any install that runs one model) refused every assistant
// candidate with nothing but a log line to say why.

// SetHealingJudge wires the check's source: whether the config assistant is
// the healing producer, and modelfamily.PairProblem's reason for its pair.
func (h *DoctorHandlers) SetHealingJudge(fn func() (wired bool, problem string)) {
	if h == nil {
		return
	}
	h.healingJudge = fn
}

func (h *DoctorHandlers) checkHealingJudgeFamily() DoctorCheck {
	const name = "healing_judge_family"
	// Not evaluated is SKIPPED, never OK (doctor_unknown_not_ok_test).
	if h == nil || h.healingJudge == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config assistant is wired as the healing producer"}
	}
	wired, problem := h.healingJudge()
	if !wired {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config assistant is wired as the healing producer"}
	}
	if problem == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "healing candidates are judged by a model of another family"}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: "workflow healing cannot judge assistant candidates: " + problem +
		". Every assistant candidate is refused (JUDGE_SAME_FAMILY); deterministic recipe candidates still run. " +
		"Set config_assistant.judge_model to a model of a different family from the assistant."}
}
