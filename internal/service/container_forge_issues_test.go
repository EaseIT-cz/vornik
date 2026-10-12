package service

import (
	"bytes"
	"context"
	"errors"
	"github.com/rs/zerolog"
	"strings"
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
	proj := &registry.Project{ID: "p", Forge: registry.ProjectForge{Provider: "github", Issues: registry.ProjectForgeIssues{LabelsEnabled: true, WritesWithoutApproval: true}}}
	registry.SeedForTest(reg, map[string]*registry.Project{"p": proj})
	p := &issuePermissionProvider{}
	r := &forgeProviderResolver{projects: reg, cache: map[string]forge.ForgeProvider{"p": p}}
	c := &Container{Registry: reg, Logger: zerolog.Nop()}
	c.verifyForgePermissions(context.Background(), r)
	if p.checks != 1 || !p.write {
		t.Fatal("issue permission check skipped after contents warning", p.checks)
	}
}

// audit 2026-10-12 finding C: an issue-write opt-in without
// forge.issues.writes_without_approval is inert. Boot asks for Issues write
// only when writes are effective, and warns once about an inert opt-in so a
// config that used to write does not go quiet unnoticed.
func TestVerifyForgeIssueWriteNeedsOperatorAcceptance(t *testing.T) {
	for _, tc := range []struct {
		accept, wantWrite, wantWarn bool
	}{{false, false, true}, {true, true, false}} {
		reg := registry.New()
		proj := &registry.Project{ID: "p", Forge: registry.ProjectForge{Provider: "github", Issues: registry.ProjectForgeIssues{CloseEnabled: true, WritesWithoutApproval: tc.accept}}}
		registry.SeedForTest(reg, map[string]*registry.Project{"p": proj})
		p := &issuePermissionProvider{}
		r := &forgeProviderResolver{projects: reg, cache: map[string]forge.ForgeProvider{"p": p}}
		var buf bytes.Buffer
		c := &Container{Registry: reg, Logger: zerolog.New(&buf)}
		c.verifyForgePermissions(context.Background(), r)
		if p.write != tc.wantWrite {
			t.Errorf("accept=%v: write check %v, want %v", tc.accept, p.write, tc.wantWrite)
		}
		if got := strings.Contains(buf.String(), "writes_without_approval"); got != tc.wantWarn {
			t.Errorf("accept=%v: inert-opt-in warning %v, want %v: %s", tc.accept, got, tc.wantWarn, buf.String())
		}
	}
}
