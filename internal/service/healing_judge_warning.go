package service

// healingJudgeWarning is the start-up line for a healing producer whose judge
// cannot judge it (GitHub #891, config-assistant design §7.3a), or "" when
// problem is "". The doctor row healing_judge_family says the same.
func healingJudgeWarning(problem string) string {
	if problem == "" {
		return ""
	}
	return "workflow healing: every config-assistant candidate will be refused (JUDGE_SAME_FAMILY): " + problem +
		"; deterministic recipe candidates still run. Set config_assistant.judge_model to a model of a different family (vornikctl doctor: healing_judge_family)"
}
