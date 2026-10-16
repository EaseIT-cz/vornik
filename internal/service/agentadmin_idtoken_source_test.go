package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #317/#119 item 8, design §22.2.16 (scan clarification GREEN 8964):
// identity stays outside write authorization. The sole reviewed exception is
// the AccountIdentity metadata key of the BrokerStandingGrant constructed
// inside brokergrants.Service.ApproveWithGrant; no identity read is exempt.
func scanIdentityAuthorization(path string, source []byte) ([]string, int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		return nil, 0, err
	}
	allowed := map[token.Pos]bool{}
	if path == "brokergrants/grants.go" {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "ApproveWithGrant" || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			receiver, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			receiverType, ok := receiver.X.(*ast.Ident)
			if !ok || receiverType.Name != "Service" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				literal, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				typ, ok := literal.Type.(*ast.SelectorExpr)
				if !ok || typ.Sel.Name != "BrokerStandingGrant" {
					return true
				}
				pkg, ok := typ.X.(*ast.Ident)
				if !ok || pkg.Name != "persistence" {
					return true
				}
				for _, elem := range literal.Elts {
					kv, ok := elem.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if ok && key.Name == "AccountIdentity" {
						allowed[key.Pos()] = true
					}
				}
				return true
			})
		}
	}
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || allowed[id.Pos()] {
			return true
		}
		switch id.Name {
		case "IDToken", "EmailFromIDToken", "AccountIdentity", "IdentitySource":
			bad = append(bad, fset.Position(id.Pos()).String()+": "+id.Name)
		}
		return true
	})
	return bad, len(allowed), nil
}
func TestIDToken_NeverAnAuthorizationInput(t *testing.T) {
	named := []string{"api/agent_tool_gate.go", "agentadmin/tool_approval.go", "agentadmin/reach.go", "service/agentadmin_reach.go"}
	scopes := map[string][]string{}
	for _, name := range named {
		scopes[name] = []string{filepath.Join("..", name)}
	}
	for _, dir := range []string{"agentapi", "brokergrants"} {
		err := filepath.WalkDir(filepath.Join("..", dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && repowalk.SkipDir(filepath.Join("..", dir), path, d.Name()) {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				scopes[dir] = append(scopes[dir], path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	examined, exempt := 0, 0
	for scope, paths := range scopes {
		// Missing/empty authorization directories must fail, not report clean.
		if len(paths) == 0 {
			t.Fatalf("authorization scope %s examined no files", scope)
		}
		for _, path := range paths {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bad, n, err := scanIdentityAuthorization(filepath.ToSlash(strings.TrimPrefix(path, "../")), source)
			if err != nil {
				t.Fatal(err)
			}
			examined++
			exempt += n
			for _, finding := range bad {
				t.Error(finding)
			}
		}
		t.Logf("authorization scope %s: examined %d non-test Go files", scope, len(paths))
	}
	if len(scopes) != 6 || examined < 8 {
		t.Fatalf("authorization denominator: scopes=%d files=%d, want all 6 scopes and at least 8 files", len(scopes), examined)
	}
	if exempt != 1 {
		t.Errorf("metadata construction exemptions=%d, want exactly 1", exempt)
	}
	t.Logf("authorization denominator: %d files, %d metadata construction exemption", examined, exempt)
}
func TestIdentityAuthorizationScan_ExactException(t *testing.T) {
	tests := []struct {
		name, path, source string
		bad, exempt        int
	}{
		{"metadata", "brokergrants/grants.go", `package brokergrants; func (s *Service) ApproveWithGrant() { g := &persistence.BrokerStandingGrant{AccountIdentity: identity}; _ = g }`, 0, 1},
		{"selector read", "brokergrants/grants.go", `package brokergrants; func (s *Service) ApproveWithGrant() { if g.AccountIdentity != "" {} }`, 1, 0},
		{"condition", "brokergrants/grants.go", `package brokergrants; func (s *Service) Cover() { if g.IdentitySource == "provider" {} }`, 1, 0},
		{"id token", "agentapi/agentapi.go", `package agentapi; func Authorize() { _ = result.IDToken; _ = EmailFromIDToken(token) }`, 2, 0},
		{"wrong receiver", "brokergrants/grants.go", `package brokergrants; func (s *Other) ApproveWithGrant() { _ = &persistence.BrokerStandingGrant{AccountIdentity: identity} }`, 1, 0},
		{"free function", "brokergrants/grants.go", `package brokergrants; func ApproveWithGrant() { _ = &persistence.BrokerStandingGrant{AccountIdentity: identity} }`, 1, 0},
		{"other function", "brokergrants/grants.go", `package brokergrants; func (s *Service) Cover() { _ = &persistence.BrokerStandingGrant{AccountIdentity: identity} }`, 1, 0},
		{"other type", "brokergrants/grants.go", `package brokergrants; func (s *Service) ApproveWithGrant() { _ = &other.Account{AccountIdentity: identity} }`, 1, 0},
		{"other file", "brokergrants/key.go", `package brokergrants; func (s *Service) ApproveWithGrant() { _ = &persistence.BrokerStandingGrant{AccountIdentity: identity} }`, 1, 0},
		{"other reference in allowed literal", "brokergrants/grants.go", `package brokergrants; func (s *Service) ApproveWithGrant() { _ = &persistence.BrokerStandingGrant{AccountIdentity: row.AccountIdentity} }`, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := scanIdentityAuthorization(tt.path, []byte(tt.source))
			if err != nil || len(got) != tt.bad || n != tt.exempt {
				t.Fatalf("bad=%v exemptions=%d err=%v, want %d/%d", got, n, err, tt.bad, tt.exempt)
			}
		})
	}
}
