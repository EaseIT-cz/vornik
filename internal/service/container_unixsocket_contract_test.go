package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The agent socket allowlist is a contract with the agent image (LLD
// cloudflare-real-ip-design §12.1, §12.7). These tests extract every daemon
// path the image's clients build and assert allowedAgentSocketPath accepts
// each one.
//
// audit 2026-10-12: the Forge issue tool (POST
// /api/v1/projects/{p}/forge/issues, entrypoint handle_forge_issue) shipped
// without an allowlist entry, so every forge_* call 404ed over the socket in
// production and the weekly issue triage was dead. Nothing checked the
// comment that says "a new agent-side call is added here". This is that
// check. It fails loudly on a construction it cannot parse rather than
// skipping it, and it reports its denominator.

const (
	agentEntrypointPath = "../../images/vornik-agent/entrypoint.sh"
	mcpBridgeMainPath   = "../../cmd/mcp-bridge/main.go"
)

// entrypointBaseRE matches a daemon URL construction in the entrypoint: one of
// the daemon base variables followed by a path. LLM_ENDPOINT is the
// entrypoint's copy of VORNIK_LLM_ENDPOINT.
var entrypointBaseRE = regexp.MustCompile(`\$\{(VORNIK_API_URL|VORNIK_MEM_URL|VORNIK_LLM_ENDPOINT|LLM_ENDPOINT)(%/)?\}(/[^"'\s;)]*)`)

// bracedVarSegRE is one path segment that is exactly one ${var} expansion.
var bracedVarSegRE = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

type agentCallSite struct {
	where string // file:line
	path  string // normalised daemon path, placeholders for variables
}

// normaliseShellDaemonPath turns the path part of a construction into a
// daemon path: the query is dropped, every ${var} segment becomes a
// placeholder, and the LLM base gets the /api/v1 prefix daemonOnlyEnv gives
// it. Anything else that still carries a shell expansion is unparseable.
func normaliseShellDaemonPath(baseVar, raw string) (string, bool) {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	segs := strings.Split(raw, "/")
	for i, s := range segs {
		if bracedVarSegRE.MatchString(s) {
			segs[i] = "x"
			continue
		}
		if strings.ContainsAny(s, "$`\\{}") {
			return "", false
		}
	}
	p := strings.Join(segs, "/")
	if baseVar == "LLM_ENDPOINT" || baseVar == "VORNIK_LLM_ENDPOINT" {
		// daemonOnlyEnv: VORNIK_LLM_ENDPOINT = unix://<sock>/api/v1.
		p = "/api/v1" + p
	}
	return p, true
}

func extractEntrypointCallSites(t *testing.T) []agentCallSite {
	t.Helper()
	raw, err := os.ReadFile(agentEntrypointPath)
	if err != nil {
		t.Fatalf("read agent entrypoint: %v", err)
	}
	var sites []agentCallSite
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		where := filepath.Base(agentEntrypointPath) + ":" + strconv.Itoa(i+1)
		matches := entrypointBaseRE.FindAllStringSubmatchIndex(line, -1)
		covered := make([]bool, len(line))
		for _, m := range matches {
			base := line[m[2]:m[3]]
			pathPart := line[m[6]:m[7]]
			for j := m[0]; j < m[1]; j++ {
				covered[j] = true
			}
			p, ok := normaliseShellDaemonPath(base, pathPart)
			if !ok {
				t.Errorf("%s: cannot parse daemon URL construction %q; extend the extractor, do not skip it", where, line[m[0]:m[1]])
				continue
			}
			sites = append(sites, agentCallSite{where: where, path: p})
		}
		// A daemon path literal outside a recognised construction is a call
		// site the extractor would otherwise miss (a URL built from another
		// variable, a hand-rolled base). Fail loudly.
		for _, lit := range []string{"/api/v1/", "/v1/chat/completions", "/chat/completions"} {
			for off := 0; ; {
				k := strings.Index(line[off:], lit)
				if k < 0 {
					break
				}
				if !covered[off+k] {
					t.Errorf("%s: daemon path %q outside a recognised ${VORNIK_API_URL|VORNIK_MEM_URL|LLM_ENDPOINT}/... construction; extend the extractor: %s", where, lit, trimmed)
				}
				off += k + len(lit)
			}
		}
	}
	return sites
}

// flattenConcat flattens a chain of string + expressions into its operands.
func flattenConcat(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return append(flattenConcat(b.X), flattenConcat(b.Y)...)
	}
	return []ast.Expr{e}
}

func extractMCPBridgeCallSites(t *testing.T) []agentCallSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, mcpBridgeMainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse mcp-bridge: %v", err)
	}
	isDaemonLit := func(e ast.Expr) bool {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return false
		}
		s, _ := strconv.Unquote(bl.Value)
		return strings.HasPrefix(s, "/api/") || strings.HasPrefix(s, "/v1/")
	}
	var sites []agentCallSite
	inChain := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || b.Op != token.ADD || inChain[b] {
			return true
		}
		ops := flattenConcat(b)
		hasDaemon := false
		for _, op := range ops {
			if isDaemonLit(op) {
				hasDaemon = true
			}
		}
		if !hasDaemon {
			return true
		}
		ast.Inspect(b, func(m ast.Node) bool {
			if e, ok := m.(ast.Expr); ok {
				inChain[e] = true
			}
			return true
		})
		where := filepath.Base(mcpBridgeMainPath) + ":" + strconv.Itoa(fset.Position(b.Pos()).Line)
		first, ok := ops[0].(*ast.Ident)
		if !ok || first.Name != "base" {
			t.Errorf("%s: daemon URL concatenation does not start from the daemonHTTP base; extend the extractor", where)
			return false
		}
		var sb strings.Builder
		for _, op := range ops[1:] {
			switch v := op.(type) {
			case *ast.BasicLit:
				s, err := strconv.Unquote(v.Value)
				if err != nil || v.Kind != token.STRING {
					t.Errorf("%s: unparseable literal %s", where, v.Value)
					return false
				}
				sb.WriteString(s)
			case *ast.Ident:
				sb.WriteString("x")
			default:
				t.Errorf("%s: unparseable operand %T in daemon URL concatenation; extend the extractor", where, op)
				return false
			}
		}
		sites = append(sites, agentCallSite{where: where, path: sb.String()})
		return false
	})
	// A daemon path literal that is not part of a parsed concatenation is a
	// call site the extractor missed.
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && isDaemonLit(e) && !inChain[e] {
			t.Errorf("%s: daemon path literal %s outside a base + ... concatenation; extend the extractor",
				filepath.Base(mcpBridgeMainPath)+":"+strconv.Itoa(fset.Position(e.Pos()).Line), e.(*ast.BasicLit).Value)
		}
		return true
	})
	return sites
}

// TestAgentSocketAllowlist_CoversEveryAgentImageCall is the contract check
// (audit 2026-10-12): every daemon path the agent image builds must pass the
// socket allowlist, or it 404s for every daemon-only role.
func TestAgentSocketAllowlist_CoversEveryAgentImageCall(t *testing.T) {
	entry := extractEntrypointCallSites(t)
	bridge := extractMCPBridgeCallSites(t)
	t.Logf("examined %d daemon URL constructions in %s and %d in %s",
		len(entry), agentEntrypointPath, len(bridge), mcpBridgeMainPath)
	// Floors guard the extractor itself: a regex that silently stopped
	// matching would otherwise report "all allowed" over zero sites. Raise
	// them when a call site is added; never lower them to pass.
	const entrypointFloor, bridgeFloor = 11, 2
	if len(entry) < entrypointFloor {
		t.Errorf("entrypoint: examined %d call sites, want at least %d; the extractor lost some", len(entry), entrypointFloor)
	}
	if len(bridge) < bridgeFloor {
		t.Errorf("mcp-bridge: examined %d call sites, want at least %d; the extractor lost some", len(bridge), bridgeFloor)
	}
	refused := 0
	for _, s := range append(entry, bridge...) {
		if !allowedAgentSocketPath(s.path) {
			refused++
			t.Errorf("%s: agent calls %s, which the unix socket allowlist refuses (404); add it to agentSocketExactPaths/agentSocketProjectSuffixes and LLD cloudflare-real-ip-design §12.1", s.where, s.path)
		}
	}
	t.Logf("%d of %d call sites refused by the allowlist", refused, len(entry)+len(bridge))
}

// The extractor's parse rules, pinned so a regression in them cannot pass
// the contract test vacuously.
func TestNormaliseShellDaemonPath(t *testing.T) {
	for _, tc := range []struct {
		base, raw, want string
		ok              bool
	}{
		{"VORNIK_API_URL", "/api/v1/projects/${project_id}/forge/issues", "/api/v1/projects/x/forge/issues", true},
		{"VORNIK_API_URL", "/api/v1/projects/${project_id}/tasks/${task_id}/messages?limit=${limit}", "/api/v1/projects/x/tasks/x/messages", true},
		{"LLM_ENDPOINT", "/chat/completions", "/api/v1/chat/completions", true},
		{"VORNIK_API_URL", "/api/v1/projects/$project_id/x", "", false},
		{"VORNIK_API_URL", "/api/v1/projects/p-${id}/x", "", false},
		{"VORNIK_API_URL", "/api/v1/$(cmd)", "", false},
	} {
		got, ok := normaliseShellDaemonPath(tc.base, tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normaliseShellDaemonPath(%q, %q) = %q, %v; want %q, %v", tc.base, tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
