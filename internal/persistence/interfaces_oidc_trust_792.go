package persistence

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// GitHub #792 (design 27): a companion key bound to one GitHub Actions
// workflow. A job presenting that run's ID token is served as the key. Every
// column is an exact match; the claim set is unique, because the token never
// names a key.

// CompanionOIDCTrust is one trust row.
type CompanionOIDCTrust struct {
	ID             string // ktrust_<24 hex>
	KeyID          string
	ProjectID      string // the key's project, filled on reads
	Issuer         string
	Audience       string
	RepositoryID   int64
	Repository     string // owner/name
	Ref            string // e.g. refs/heads/main
	JobWorkflowRef string // owner/name/.github/workflows/<file>@<ref>
	EventNames     []string
	CreatedAt      time.Time
	CreatedBy      string
	LastUsedAt     *time.Time
}

// ErrOIDCTrustDuplicate is returned when a trust with the same claim set
// exists (design 27 §C1).
var ErrOIDCTrustDuplicate = errors.New("a trust with the same claims already exists")

// OIDCTrustsOverlap reports whether a and b could both match one token: the
// same claims but the events, and at least one event in common.
func OIDCTrustsOverlap(a, b CompanionOIDCTrust) bool {
	if a.Issuer != b.Issuer || a.Audience != b.Audience || a.RepositoryID != b.RepositoryID ||
		a.Repository != b.Repository || a.Ref != b.Ref || a.JobWorkflowRef != b.JobWorkflowRef {
		return false
	}
	for _, x := range a.EventNames {
		for _, y := range b.EventNames {
			if x == y {
				return true
			}
		}
	}
	return false
}

// CompanionOIDCTrustRepository persists trusts.
type CompanionOIDCTrustRepository interface {
	// CreateOIDCTrust inserts t; ErrOIDCTrustDuplicate when its claim set
	// exists. EventNames are stored sorted.
	CreateOIDCTrust(ctx context.Context, t CompanionOIDCTrust) error
	// OIDCTrustsForRepository returns the trusts for one issuer and
	// repository id: the candidates for a token.
	OIDCTrustsForRepository(ctx context.Context, issuer string, repositoryID int64) ([]CompanionOIDCTrust, error)
	// OIDCTrustsForProject lists the trusts on a project's keys.
	OIDCTrustsForProject(ctx context.Context, projectID string) ([]CompanionOIDCTrust, error)
	// DeleteOIDCTrust removes one trust of projectID; false when none.
	DeleteOIDCTrust(ctx context.Context, projectID, id string) (bool, error)
	// TouchOIDCTrust sets last_used_at.
	TouchOIDCTrust(ctx context.Context, id string, at time.Time) error
}

// EncodeOIDCEventNames is the stored form of an event list: sorted, unique,
// comma-joined, so the unique index compares it as a set.
func EncodeOIDCEventNames(names []string) string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// DecodeOIDCEventNames reverses EncodeOIDCEventNames.
func DecodeOIDCEventNames(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
