package ui

// Console saves check each model they change before writing it (GitHub #881,
// project and swarm registry design §12.2 C2 and C3).

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"vornik.io/vornik/internal/modelcheck"
	"vornik.io/vornik/internal/persistence"
)

// WithModelChecker installs the check a console save runs before it writes a
// model, and, on a managed instance, the catalogue a model must be in (nil:
// not managed).
func WithModelChecker(c *modelcheck.Checker, managedCatalogue func() []string) ServerOption {
	return func(s *Server) {
		s.modelChecker = c
		s.managedCatalogue = managedCatalogue
	}
}

// checkSavedModels checks each model in after that is not in before. It
// returns the warnings to show with a save, or the refusal to show instead
// of saving. On a managed instance a model outside the catalogue is refused
// even with "Save without checking", which skips only the call.
func (s *Server) checkSavedModels(r *http.Request, before, after []string) (warnings []string, refusal string) {
	changed := changedModels(before, after)
	if len(changed) == 0 {
		return nil, ""
	}
	if s.managedCatalogue != nil {
		cat := s.managedCatalogue()
		for _, m := range changed {
			if !slices.Contains(cat, m) {
				return nil, m + " is not in this instance's model catalogue, so it cannot be saved"
			}
		}
	}
	if v := r.FormValue("skipModelCheck"); v == "on" || v == "1" || v == "true" {
		s.writeModelCheckSkippedAudit(r, changed)
		return []string{"Models not checked (Save without checking): " + strings.Join(changed, ", ")}, ""
	}
	if s.modelChecker == nil {
		return nil, ""
	}
	res := s.modelChecker.CheckAll(r.Context(), changed)
	for _, m := range changed {
		switch rr := res[m]; {
		case !rr.OK:
			return nil, rr.Reason + ". Nothing was saved; fix the model, or tick Save without checking."
		case rr.Warning != "":
			warnings = append(warnings, rr.Warning)
		}
	}
	return warnings, ""
}

// changedModels is the sorted, distinct non-empty models in after that are
// not in before.
func changedModels(before, after []string) []string {
	had := map[string]bool{}
	for _, m := range before {
		had[m] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range after {
		if m == "" || had[m] || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// fileSwarmModels is every model the swarm file at path names itself.
func fileSwarmModels(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return modelcheck.SwarmFileModels(raw, path)
}

var schemaModelField = regexp.MustCompile(`^roles\[\d+\]\.(model|modelFallback)$`)

// schemaFormModels is every role model and fallback in a schema-editor form.
func schemaFormModels(r *http.Request) []string {
	keys := make([]string, 0, len(r.Form))
	for k := range r.Form {
		if schemaModelField.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if v := r.FormValue(k); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) writeModelCheckSkippedAudit(r *http.Request, models []string) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		principal = "ui-admin"
	}
	_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal, Source: "ui", Action: "model.check_skipped",
		Target: strings.Join(models, ", "), IP: clientIP(r), UserAgent: r.UserAgent(),
	})
}
