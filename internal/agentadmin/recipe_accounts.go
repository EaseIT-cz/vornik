package agentadmin

import (
	"fmt"
	"sort"
	"strings"
)

// Recipes per account (agent-administered design §22.2.8; GitHub #119 item
// 8). An install as another of the user's accounts renders the recipe with
// every name it owns suffixed by the label, so two accounts never share a
// server, a credential, a workflow or a role.

// RecipeAccountsRule is what list_recipes says about accounts.
const RecipeAccountsRule = "May be installed once per account: install it again with account <label> for another of the user's accounts; " +
	"that install gets its own server <need>-<label>, credential <NAME>_<LABEL>, roles <recipe>-<label>-<role> and workflow <slug>-<label>."

// forAccount is the recipe as installed for label: a copy with each need's
// server and credential named for the account, the role tools and the
// instructions that name a server rewritten to the account's server, the
// workflow slug suffixed, and the egress (whose item sources are the need
// names) merged again. A name past its rule is refused, naming the label.
func (r *Recipe) forAccount(env Envelope, label string) (*Recipe, error) {
	cp := *r
	cp.account = label
	cp.baseSlug = r.Workflow.Slug
	upper := strings.ToUpper(strings.ReplaceAll(label, "-", "_"))
	tooLong := func(what, name string) error {
		return fmt.Errorf("the account label %q makes the %s %q too long; use a shorter label", label, what, name)
	}
	type rename struct{ from, to string }
	var renames []rename
	cp.Needs = make([]RecipeNeed, len(r.Needs))
	for i, n := range r.Needs {
		nn := n
		nn.Name = n.Name + "-" + label
		if !nameRe.MatchString(nn.Name) {
			return nil, tooLong("server name", nn.Name)
		}
		nn.ReadTools = append([]string(nil), n.ReadTools...)
		if n.Credential != nil {
			c := *n.Credential
			if c.Kind == CredentialOAuth {
				c.Name = OAuthCredentialName(nn.Name)
			} else {
				c.Name = n.Credential.Name + "_" + upper
			}
			if !credentialRe.MatchString(c.Name) {
				return nil, tooLong("credential name", c.Name)
			}
			nn.Credential = &c
		}
		renames = append(renames, rename{"mcp__" + n.Name + "__", "mcp__" + nn.Name + "__"})
		cp.Needs[i] = nn
	}
	rewrite := func(s string) string {
		for _, rn := range renames {
			s = strings.ReplaceAll(s, rn.from, rn.to)
		}
		return s
	}
	// A label that begins a team role's own name would make a role of the
	// catalogue's install read as this account's (ownsRole picks the
	// longest prefix): refused (lane B review aae3 finding 4).
	for _, role := range r.Team {
		if strings.HasPrefix(role.Name, label+"-") || role.Name == label {
			return nil, fmt.Errorf("the account label %q is also the start of the recipe's role %q; use another label", label, role.Name)
		}
	}
	cp.Team = make([]RecipeRole, len(r.Team))
	for i, role := range r.Team {
		nr := role
		nr.Instructions = rewrite(role.Instructions)
		nr.Tools = make([]string, len(role.Tools))
		for j, t := range role.Tools {
			nr.Tools[j] = rewrite(t)
		}
		if !nameRe.MatchString(cp.RoleName(role.Name)) {
			return nil, tooLong("role name", cp.RoleName(role.Name))
		}
		cp.Team[i] = nr
	}
	cp.Workflow = r.Workflow
	cp.Workflow.Slug = r.Workflow.Slug + "-" + label
	if !slugRe.MatchString(cp.Workflow.Slug) {
		return nil, tooLong("workflow slug", cp.Workflow.Slug)
	}
	cp.Workflow.Steps = make([]RecipeStep, len(r.Workflow.Steps))
	for i, st := range r.Workflow.Steps {
		ns := st
		ns.Instructions = rewrite(st.Instructions)
		cp.Workflow.Steps[i] = ns
	}
	if err := cp.finishEgress(env); err != nil {
		return nil, err
	}
	return &cp, nil
}

// baseWorkflowSlug is the catalogue's workflow slug, before any account.
func (r *Recipe) baseWorkflowSlug() string {
	if r.baseSlug != "" {
		return r.baseSlug
	}
	return r.Workflow.Slug
}

// accountOfInstall is the account label of an installed workflow of the
// recipe, read from its slug: "" for the catalogue's own install.
func accountOfInstall(rec *Recipe, project, workflowID string) string {
	slug := strings.TrimPrefix(workflowID, project+"--")
	label, ok := strings.CutPrefix(slug, rec.baseWorkflowSlug()+"-")
	if !ok {
		return ""
	}
	return label
}

// installedWorkflowsOf are the project's workflows installed from the
// recipe (as any account), so role ownership can tell the installs apart.
func installedWorkflowsOf(st *State, project, recipe string) []string {
	var out []string
	for _, wid := range st.workflowsOf(project) {
		if name, _ := InstalledRecipe(st.Workflows[wid].Loaded); name == recipe {
			out = append(out, wid)
		}
	}
	sort.Strings(out)
	return out
}

// ownsRole reports whether a role of the project belongs to this install:
// the longest role prefix among the recipe's installs (the catalogue's own,
// <recipe>-, and each <recipe>-<label>- of an install in the project) that
// the name carries is this install's. So a default install never claims
// agenda-work-reader, and a work install never claims agenda-work-x-reader.
func ownsRole(rec *Recipe, installedWorkflows []string, role string) bool {
	prefixes := []string{rec.Name + "-", rec.rolePrefix()}
	for _, wid := range installedWorkflows {
		project := wid[:strings.LastIndex(wid, "--")]
		if label := accountOfInstall(rec, project, wid); label != "" {
			prefixes = append(prefixes, rec.Name+"-"+label+"-")
		}
	}
	best := ""
	for _, p := range prefixes {
		if strings.HasPrefix(role, p) && len(p) > len(best) {
			best = p
		}
	}
	return best != "" && best == rec.rolePrefix()
}
