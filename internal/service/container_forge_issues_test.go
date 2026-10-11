package service

import (
	"context"
	"errors"
	"github.com/rs/zerolog"
	"testing"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

type issuePermissionProvider struct {
	forge.ForgeProvider
	checks int
	write  bool
}

func (p *issuePermissionProvider) VerifyPushAccess(context.Context) error {
	return errors.New("missing contents")
}
func (p *issuePermissionProvider) ListIssues(context.Context, string, forge.IssueFilter) (forge.IssueList, error) {
	return forge.IssueList{}, nil
}
func (p *issuePermissionProvider) GetIssue(context.Context, string, int) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (p *issuePermissionProvider) ListLabels(context.Context, string) (forge.LabelList, error) {
	return forge.LabelList{}, nil
}
func (p *issuePermissionProvider) VerifyIssueAccess(_ context.Context, write bool) error {
	p.checks++
	p.write = write
	return errors.New("missing issues")
}
func TestVerifyForgeIssuePermissionDespitePushFailure(t *testing.T) {
	reg := registry.New()
	proj := &registry.Project{ID: "p", Forge: registry.ProjectForge{Provider: "github", Issues: registry.ProjectForgeIssues{LabelsEnabled: true}}}
	registry.SeedForTest(reg, map[string]*registry.Project{"p": proj})
	p := &issuePermissionProvider{}
	r := &forgeProviderResolver{projects: reg, cache: map[string]forge.ForgeProvider{"p": p}}
	c := &Container{Registry: reg, Logger: zerolog.Nop()}
	c.verifyForgePermissions(context.Background(), r)
	if p.checks != 1 || !p.write {
		t.Fatal("issue permission check skipped after contents warning", p.checks)
	}
}
