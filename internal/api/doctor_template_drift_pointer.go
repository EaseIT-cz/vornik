package api

// The config_template_drift pointer (drift design, "The check only pays off
// through the messages of checks that already fire"; round 5 F2).
//
// Issue #61(a)'s reporter was not missing a check: workflow_onfail_masking
// fired correctly, and nothing told them their deployed workflow predated the
// marker it honours. So a row whose findings name the content of a deployed
// config file carries a pointer to config_template_drift — appended by ONE
// post-processing step after every check has run, so no check can forget it,
// and only while config_template_drift is WARNING: never when it is SKIPPED
// (it would point at a check that did not run — the CE default until the
// package routes run the installer) and never when it is OK.

// templateDriftPointer is the sentence appended to a registry row.
const templateDriftPointer = " — config_template_drift reports deployed configs that differ from what this version ships; " +
	"if this finding is about one of them, the fix may already be in the template (see that row)."

// doctorCheckConfigPointer classifies EVERY check method RunDoctor calls: the
// check name when its findings name content of a deployed config file (tunable
// or canonical — the rows that get the pointer), "" when they do not. A test
// parses RunDoctor and fails when a called check is missing here, so adding a
// check forces the decision rather than defaulting it.
var doctorCheckConfigPointer = map[string]string{
	// Findings about deployed config content — the registry.
	"checkWorkflowOnFailMasking": "workflow_onfail_masking",
	"checkWorkflowMDShape":       "workflow_md_shape",
	"checkRolePromptSanity":      "role_prompt_sanity",
	"checkWorkflowSwarmCompat":   "workflow_swarm_compat",
	"checkDispatcherRole":        "dispatcher_role",
	"checkRoleLibrary":           "role_library",

	// Do not report on the content of a deployed config file (runtime state,
	// the database, images, secrets, the host, or config.yaml itself).
	"checkConfigTemplateDrift":     "",
	"checkAgentImages":             "",
	"checkAgentImageUID":           "",
	"checkAgentLLMAPIKey":          "",
	"checkAgentLLMTopology":        "",
	"checkAgentModelCircuits":      "",
	"checkAPIKeyStrength":          "",
	"checkAPISecurityPosture":      "",
	"checkAutonomyBudgetGuard":     "",
	"checkBreachDeadlines":         "",
	"checkBudgetUtilisation":       "",
	"checkBuildProvenance":         "",
	"checkConfigClassCompat":       "",
	"checkConfigCRLF":              "",
	"checkConfigSecretHygiene":     "",
	"checkConfigValidation":        "",
	"checkConnectorAuth":           "",
	"checkCostAttribution":         "",
	"checkDatabaseSchema":          "",
	"checkEnvFileFreshness":        "",
	"checkEvalSuiteLint":           "",
	"checkFallbackRungs":           "",
	"checkGatewayHealthy":          "",
	"checkGitConfigComposition":    "",
	"checkImageFreshness":          "",
	"checkLeaderLocksHealth":       "",
	"checkModelCallsLive":          "",
	"checkModelCircuits":           "",
	"checkModelHealth":             "",
	"checkModelRouteCoverage":      "",
	"checkOrphanedWatchers":        "",
	"checkOrphanFKRows":            "",
	"checkOrphanWorktrees":         "",
	"checkPodmanConfig":            "",
	"checkPricingCoverage":         "",
	"checkPricingDrift":            "",
	"checkProjectConfigSkew":       "",
	"checkProjectDependencies":     "",
	"checkRetentionEnabled":        "",
	"checkSchemaGateDrift":         "",
	"checkScraperProfileFreshness": "",
	"checkSecretsPermissions":      "",
	"checkSlackSlashCommand":       "",
	"checkStaleLeases":             "",
	"checkStuckExecutions":         "",
	"checkTaskStateAudit":          "",
	"checkToolAuditRedaction":      "",
	"checkUnclassifiedShare":       "",
	"checkWebWritesInsecure":       "",
	"checkWorkspaceCanonical":      "",
}

// appendTemplateDriftPointer adds the pointer to every non-OK registry row,
// when and only when config_template_drift is WARNING in the same report.
func appendTemplateDriftPointer(checks []DoctorCheck) {
	drifting := false
	for _, c := range checks {
		if c.Name == "config_template_drift" && c.Status == "WARNING" {
			drifting = true
		}
	}
	if !drifting {
		return
	}
	registry := map[string]bool{}
	for _, name := range doctorCheckConfigPointer {
		if name != "" {
			registry[name] = true
		}
	}
	for i := range checks {
		c := &checks[i]
		if registry[c.Name] && c.Status != "OK" && c.Status != "SKIPPED" {
			c.Message += templateDriftPointer
		}
	}
}
