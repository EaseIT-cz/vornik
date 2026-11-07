package memory

import (
	"context"
	"fmt"
)

// SensitivityCapCounts is doctor's denominator for a role's sensitivity cap:
// a project's live chunks in the role's repo scope, by tier, and how many the
// role's whole filter admits.
// #618 C4; firewall design, "Amendment 2026-11-06 (#618)".
type SensitivityCapCounts struct {
	Public, Internal, Confidential, Restricted int // set by an operator
	DerivedPublic                              int // no tier, proven published
	Unlabelled                                 int // no tier and not derived public: ranks as confidential
	Admitted                                   int // what the role's filter, cap included, admits
}

// SensitivityCapCounts counts one project's live chunks (published, not
// refuted or superseded) in sf's repo scope. Admitted applies the whole of sf,
// the same fragment every search statement uses, so it is the number of
// chunks the role could retrieve, whatever it asks.
func (r *Repository) SensitivityCapCounts(ctx context.Context, projectID string, sf ScopeFilter) (SensitivityCapCounts, error) {
	derived := derivedPublicSQL(sf.PublicPrefixes)
	q := fmt.Sprintf(`
SELECT COUNT(*) FILTER (WHERE sensitivity_tier = 'public'),
       COUNT(*) FILTER (WHERE sensitivity_tier = 'internal'),
       COUNT(*) FILTER (WHERE sensitivity_tier = 'confidential'),
       COUNT(*) FILTER (WHERE sensitivity_tier = 'restricted'),
       COUNT(*) FILTER (WHERE %s),
       COUNT(*) FILTER (WHERE sensitivity_tier IS NULL AND NOT COALESCE(%s, FALSE)),
       COUNT(*) FILTER (WHERE TRUE %s)
FROM project_memory_chunks
WHERE project_id = $1
  AND lifecycle_state = 'published'
  AND validation_status NOT IN ('refuted','superseded')
  %s`, derived, derived, sf.sql(2), scopeFilterSQL(sf.Strict, 2))
	var c SensitivityCapCounts
	err := r.db.QueryRowContext(ctx, q, projectID, nullableString(sf.RepoScope)).Scan(
		&c.Public, &c.Internal, &c.Confidential, &c.Restricted, &c.DerivedPublic, &c.Unlabelled, &c.Admitted)
	if err != nil {
		return SensitivityCapCounts{}, fmt.Errorf("sensitivity cap counts for %s: %w", projectID, err)
	}
	return c, nil
}
