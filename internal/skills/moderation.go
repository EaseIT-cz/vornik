// Package skills holds channel-neutral knowledge-skill business logic
// shared by every approval surface (companion MCP, Telegram, Slack, Web
// UI) so they can't diverge (LLD 2026-07-07-knowledge-skill-learning-
// loop-design). Per-surface authorization is the caller's job; this
// package only applies an already-authorized decision.
package skills

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Decision is an approve/reject verdict from an authorized approver.
type Decision int

const (
	// Approve promotes a draft to active.
	Approve Decision = iota
	// Reject retires a skill.
	Reject
)

// ApplyDecision applies an already-authorized approve/reject to a skill
// and returns the resulting maturity. Idempotent: acting on a skill
// that already holds the target state is a no-op that reports the
// current maturity. Rejecting an active/trusted skill credits a
// "corrected" maturity signal; rejecting a draft does not.
//
// The CALLER must have verified the actor is authorized to moderate
// this skill (an allowed operator / SkillAdmin) before calling.
func ApplyDecision(ctx context.Context, repo persistence.SkillRepository, skillID string, d Decision) (string, error) {
	s, err := repo.GetByID(ctx, skillID)
	if err != nil {
		return "", err
	}
	return ApplyDecisionForVersion(ctx, repo, skillID, s.Version, d)
}

// ApplyDecisionForVersion authorizes the displayed revision, atomically refusing a re-proposal.
func ApplyDecisionForVersion(ctx context.Context, repo persistence.SkillRepository, skillID string, version int, d Decision) (string, error) {
	if version <= 0 {
		return "", fmt.Errorf("review version must be positive")
	}
	s, err := repo.GetByID(ctx, skillID)
	if err != nil {
		return "", err
	}
	if s.Version != version {
		return "", superseded(s.Version)
	}
	target := persistence.SkillMaturityActive
	if d == Reject {
		target = persistence.SkillMaturityRetired
	} else if s.Maturity == persistence.SkillMaturityTrusted {
		target = s.Maturity
	}
	if err := repo.SetMaturityForVersion(ctx, skillID, version, target); err != nil {
		if errors.Is(err, persistence.ErrSkillRevisionConflict) {
			current, getErr := repo.GetByID(ctx, skillID)
			if getErr != nil {
				return "", getErr
			}
			return "", superseded(current.Version)
		}
		return "", err
	}
	return target, nil
}

func superseded(version int) error {
	return fmt.Errorf("%w: Superseded by v%d; review the newer proposal", persistence.ErrSkillRevisionConflict, version)
}

// ReviewToken encodes revision identity for bounded channel callbacks.
func ReviewToken(id string, version int) string { return id + ":" + strconv.Itoa(version) }

// ParseReviewToken refuses historic ID-only buttons: their displayed revision is unknown.
func ParseReviewToken(token string) (string, int, error) {
	id, raw, ok := strings.Cut(token, ":")
	v, err := strconv.Atoi(raw)
	if !ok || id == "" || err != nil || v <= 0 {
		return "", 0, fmt.Errorf("this review predates revision binding; open the current proposal")
	}
	return id, v, nil
}

// ProposalDate labels unavailable and estimated historic dates honestly.
func ProposalDate(proposedAt time.Time, estimated bool) string {
	if proposedAt.IsZero() {
		return "Unknown (legacy proposal date)"
	}
	date := proposedAt.UTC().Format("2006-01-02 15:04:05 UTC")
	if estimated {
		date += " (legacy estimate)"
	}
	return date
}
