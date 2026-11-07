package service

import (
	"context"
	"sort"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/registry"
)

// sensitivityCapCounter is the memory repository's SensitivityCapCounts.
type sensitivityCapCounter func(ctx context.Context, projectID string, sf memory.ScopeFilter) (memory.SensitivityCapCounts, error)

// sensitivityCapRoles lists every role of a loaded project whose memorySearch
// policy sets maxSensitivity (#618 C4), with its project's counts under the
// role's whole filter: the same ScopeFilter the role's searches get.
func sensitivityCapRoles(ctx context.Context, projects []*registry.Project, swarmOf func(projectID string) (*registry.Swarm, error),
	publicPrefixes []string, count sensitivityCapCounter) []api.SensitivityCapRole {
	var out []api.SensitivityCapRole
	for _, p := range projects {
		sw, err := swarmOf(p.ID)
		if err != nil || sw == nil {
			continue
		}
		for _, r := range sw.Roles {
			pol := r.MemorySearch
			if pol == nil || pol.MaxSensitivity == "" {
				continue
			}
			authority := pol.Authority
			if authority == registry.MemorySearchAuthorityAny {
				authority = memory.AuthorityAny
			}
			row := api.SensitivityCapRole{ProjectID: p.ID, Role: r.Name, MaxSensitivity: pol.MaxSensitivity}
			c, cerr := count(ctx, p.ID, memory.ScopeFilter{RepoScope: pol.RepoScope, Strict: pol.StrictScope, Authority: authority,
				SourcePrefixes: pol.SourcePrefixes, MaxSensitivity: pol.MaxSensitivity, PublicPrefixes: publicPrefixes})
			if cerr != nil {
				row.Err = cerr
			} else {
				row.Public, row.Internal, row.Confidential, row.Restricted = c.Public, c.Internal, c.Confidential, c.Restricted
				row.DerivedPublic, row.Unlabelled, row.Admitted = c.DerivedPublic, c.Unlabelled, c.Admitted
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].Role < out[j].Role
	})
	return out
}
