package service

// GitHub #602 (design 11, amendment 2026-11-20, B4): autonomy_evaluations
// gains a redaction seam because an observe-mode WOULD_START row stores the
// prompt the tick would have submitted. The seam follows the others' rules:
// installed in every branch, a bypass carries its reason, and a re-run keeps
// a scanning decorator and upgrades a bypass without nesting.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auditredact"
)

func TestAutonomyEvaluationsSeam602_InstalledBypassedAndUpgraded(t *testing.T) {
	c := contentSeamContainer(t, false)
	c.initToolAuditRedaction()
	ae, ok := c.repos.AutonomyEvaluations.(*auditredact.AutonomyEvaluations)
	require.True(t, ok, "autonomy_evaluations left undecorated: %T", c.repos.AutonomyEvaluations)
	require.Equal(t, auditredact.ReasonSecretsDisabled, ae.BypassReason())

	c.initToolAuditRedaction()
	require.Same(t, ae, c.repos.AutonomyEvaluations.(*auditredact.AutonomyEvaluations), "a same-reason bypass is kept")

	c.Config.Secrets.Enabled = true
	c.initToolAuditRedaction()
	up := c.repos.AutonomyEvaluations.(*auditredact.AutonomyEvaluations)
	require.Equal(t, auditredact.ReasonNone, up.BypassReason(), "the bypass was not upgraded")
	_, nested := up.Inner().(*auditredact.AutonomyEvaluations)
	require.False(t, nested, "the upgrade nested the seam over the bypass")

	c.initToolAuditRedaction()
	require.Same(t, up, c.repos.AutonomyEvaluations.(*auditredact.AutonomyEvaluations), "a scanning decorator must not be wrapped again")
}
