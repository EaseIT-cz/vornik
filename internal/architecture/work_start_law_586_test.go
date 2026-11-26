package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// The work-start law (GitHub #586; deploy idle gate design §13.8 item 4,
// §13.11.2). A held start must start no work, and the held start defers
// exactly one function, Container.startWork. So every place in the daemon's
// start code that starts something (a `go` statement, or a call to a method
// named Start, Run or Recover) must sit in a function reached from
// startWork, or be named below with the reason it is not work. A new
// starter fails this test until it is placed.
//
// Scope: non-test files of internal/service and
// internal/enterprise/*/subsystem*.go. A call into another package
// (Scheduler.Start, bot.Start, graphWorker.Start) is one unit: what the
// callee starts inside starts only when the call does.
//
// Reachability is a static walk by name. A call on the Container (c.x(),
// by this package's naming) resolves to the Container method x. Any other
// selector call (s.Start(), c.pusher.Run()) resolves to every non-Container
// method of that name in scope: the subsystems' Start methods, reached
// through startSubsystems. A plain call resolves to the function of that
// name. It over-approximates what startWork reaches. It cannot follow a
// function value stored in a field and called later; such a starter is
// attributed to the function that builds it, which is then unplaced.
//
// What it cannot see: a goroutine a constructor in another package starts
// by itself. The second rule lists every `go` statement inside a function
// named New* in non-test internal/ code.
//
// Found at: review (#586 as-built §12: 0 of 45 work starters read the
// drain). Regression test at: unit (architecture).

// workStartNotWork names the units outside startWork's reach, keyed
// "<file>:<function>", with why each is not work (§12.6) or runs on a
// request or signal rather than at start (§12.2 "Not examined").
var workStartNotWork = map[string]string{
	// The listeners (L1 to L5) and the start sequence itself.
	"container.go:Run": "service.Run: the SIGHUP goroutine, which defers while held (reloadOnSignal)",
	"container_listeners.go:Container.startListeners": "L1 to L4, each behind the hold gate",
	"container.go:Container.serveAgentUnixSocket":     "L5, behind the hold gate (refuses everything while held)",
	"shutdown_http.go:shutdownHTTPServers":            "shutdown: stops the listeners",
	// §12.6: not work. P reaches outside the process; R stays in it, read-only.
	"container_doctor_gauge.go:Container.startDoctorGauge":    "P: read-only doctor report on an interval",
	"container_mcp_recovery.go:Container.startMCPReconnector": "P: MCP failed-dial retries and agent relist",
	"container_subsystems.go:Container.initMCPRegistry":       "P: the MCP registry's first refresh",
	"container_subsystems.go:Container.refreshMCPRegistry":    "P: an MCP registry refresh (the reload activator, the first refresh)",
	"container_logship.go:Container.initLogship":              "P: log shipping",
	"container_observability.go:Container.initStateCollector": "R: state collector",
	"container_quality.go:Container.initQualityCollector":     "R: quality collector",
	"container_subsystems.go:Container.initDatabase":          "R: DB metrics collector",
	"container.go:NewContainerWithObservability":              "R: DB metrics collector",
	"container_scheduler.go:Container.initScheduler":          "R: warm-pool reaper (stops idle containers, starts none)",
	"container_subsystems.go:Container.initTelegram":          "R: scraper block-notify, which only reacts to blocks an MCP call (work) hit",
	// Started by a request, never at start (§12.2 "Not examined"); while
	// held the hold gate refuses the request.
	"agentadmin_service.go:agentAdminService.background":     "request: an agent-admin change's background apply",
	"container_alerts.go:runAlert":                           "request: an alert raised by a handler",
	"container_mcp_oauth.go:Container.onMCPGrantChanged":     "request: an MCP OAuth grant change",
	"agentadmin_connectors.go:connectorPages.startSignIn":    "request: a connector sign-in started from the approver page",
	"email_auto_extractor.go:emailAutoExtractor.AutoExtract": "request: an input artifact extracted while a task is submitted (api extract_inputs.go)",
}

type workStartUnit struct {
	key  string // file:function
	what string
	pos  string
}

func workStartScopeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	svc, err := filepath.Glob(filepath.Join(root, "internal", "service", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	ee, err := filepath.Glob(filepath.Join(root, "internal", "enterprise", "*", "subsystem*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(svc, ee...) {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files
}

func recvTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	tx := fd.Recv.List[0].Type
	if st, ok := tx.(*ast.StarExpr); ok {
		tx = st.X
	}
	if id, ok := tx.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func recvVarName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return ""
	}
	return fd.Recv.List[0].Names[0].Name
}

// containerSelector reports whether x names the Container: the receiver of
// a Container method, or, by this package's convention, a field or
// variable named c or container (a subsystem's c := s.c; c.startTuneWorker).
func containerSelector(x ast.Expr, inContainer bool, recv string) bool {
	switch v := x.(type) {
	case *ast.Ident:
		return (inContainer && v.Name == recv) || v.Name == "c" || v.Name == "container"
	case *ast.SelectorExpr:
		return v.Sel.Name == "c" || v.Sel.Name == "container"
	}
	return false
}

type workStartDecl struct {
	file string
	fd   *ast.FuncDecl
}

func (d workStartDecl) key() string {
	if r := recvTypeName(d.fd); r != "" {
		return filepath.Base(d.file) + ":" + r + "." + d.fd.Name.Name
	}
	return filepath.Base(d.file) + ":" + d.fd.Name.Name
}

func TestWorkStartLaw586_EveryStarterIsReachedFromStartWorkOrNamed(t *testing.T) {
	root := moduleRoot(t)
	files := workStartScopeFiles(t, root)
	fset := token.NewFileSet()
	var decls []workStartDecl
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls = append(decls, workStartDecl{file: f, fd: fd})
			}
		}
	}
	// Resolution tables.
	containerMethods := map[string][]int{}
	otherMethods := map[string][]int{}
	funcs := map[string][]int{}
	for i, d := range decls {
		switch recvTypeName(d.fd) {
		case "":
			funcs[d.fd.Name.Name] = append(funcs[d.fd.Name.Name], i)
		case "Container":
			containerMethods[d.fd.Name.Name] = append(containerMethods[d.fd.Name.Name], i)
		default:
			otherMethods[d.fd.Name.Name] = append(otherMethods[d.fd.Name.Name], i)
		}
	}
	starts := containerMethods["startWork"]
	if len(starts) != 1 {
		t.Fatalf("Container.startWork found %d times, want 1", len(starts))
	}
	reached := map[int]bool{}
	queue := append([]int(nil), starts...)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if reached[i] {
			continue
		}
		reached[i] = true
		d := decls[i]
		recv := recvVarName(d.fd)
		isContainer := recvTypeName(d.fd) == "Container"
		ast.Inspect(d.fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				queue = append(queue, funcs[fn.Name]...)
			case *ast.SelectorExpr:
				if containerSelector(fn.X, isContainer, recv) {
					queue = append(queue, containerMethods[fn.Sel.Name]...)
				} else {
					queue = append(queue, otherMethods[fn.Sel.Name]...)
				}
			}
			return true
		})
	}

	var units, unplaced []workStartUnit
	for i, d := range decls {
		key := d.key()
		ast.Inspect(d.fd.Body, func(n ast.Node) bool {
			var what string
			switch x := n.(type) {
			case *ast.GoStmt:
				what = "go statement"
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "Start", "Run", "Recover":
						what = "call ." + sel.Sel.Name
					}
				}
			}
			if what == "" {
				return true
			}
			u := workStartUnit{key: key, what: what, pos: fset.Position(n.Pos()).String()}
			units = append(units, u)
			if !reached[i] {
				if _, named := workStartNotWork[key]; !named {
					unplaced = append(unplaced, u)
				}
			}
			return true
		})
	}
	if !requireExamined(t, "work-start law (#586)", "starter units in "+strconv.Itoa(len(files))+" files", len(units), 80) {
		return
	}
	inReach := 0
	for _, u := range units {
		for i, d := range decls {
			if reached[i] && d.key() == u.key {
				inReach++
				break
			}
		}
	}
	t.Logf("work-start law (#586): %d units, %d in functions reached from startWork, %d named not-work entries, %d unplaced",
		len(units), inReach, len(workStartNotWork), len(unplaced))
	for _, u := range unplaced {
		t.Errorf("%s (%s at %s) is not reached from startWork and not named in workStartNotWork; "+
			"start it from startWork, or name it with why it is not work (deploy idle gate design §13.8 item 4)", u.key, u.what, u.pos)
	}
	// A named entry that no longer names a unit is stale.
	seen := map[string]bool{}
	for _, u := range units {
		seen[u.key] = true
	}
	for k := range workStartNotWork {
		if !seen[k] {
			t.Errorf("workStartNotWork names %s, which starts nothing in scope any more; remove it", k)
		}
	}
	// The mutation guard of §13.8: the work starters the design names are
	// reached. Moving one out of startWork fails here.
	for _, must := range []string{"startAutonomy", "startSubsystems", "startErasureEscalation"} {
		ok := false
		for _, i := range containerMethods[must] {
			ok = ok || reached[i]
		}
		if !ok {
			t.Errorf("Container.%s is not reached from startWork", must)
		}
	}
}

// workStartNewGoroutines names every `go` statement inside a function
// named New* in non-test internal/ code: a constructor that starts a
// goroutine runs at construction, before any hold can defer it. Keyed
// "<path from internal/>:<function>"; measured on the first run (2026-11-24).
var workStartNewGoroutines = map[string]string{
	"executor/livepubsub/db_publisher.go:NewDBBacked":    "R: the live-events LISTEN loop (in-process pub/sub plumbing)",
	"executor/livepubsub/publisher.go:NewWithSweeper":    "R: the live-events subscriber sweeper",
	"service/container.go:NewContainerWithObservability": "R: the DB metrics collector",
}

func TestWorkStartLaw586_ConstructorsStartNoGoroutineUnnamed(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	examined := 0
	var found, unparsed []string
	walkRoot := filepath.Join(root, "internal")
	err := filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(walkRoot, p, d.Name(), "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		af, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			unparsed = append(unparsed, p) // reported below: not examined is not clean
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal"), p)
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "New") {
				continue
			}
			examined++
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.GoStmt); ok {
					found = append(found, filepath.ToSlash(rel)+":"+fd.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range unparsed {
		t.Errorf("%s does not parse, so its constructors were not examined", p)
	}
	if !requireExamined(t, "work-start law, constructors (#586)", "New* functions", examined, 200) {
		return
	}
	seen := map[string]bool{}
	for _, k := range found {
		seen[k] = true
		if _, ok := workStartNewGoroutines[k]; !ok {
			t.Errorf("%s starts a goroutine in a constructor; name it in workStartNewGoroutines with why it is not work, or start it later", k)
		}
	}
	for k := range workStartNewGoroutines {
		if !seen[k] {
			t.Errorf("workStartNewGoroutines names %s, which starts no goroutine any more; remove it", k)
		}
	}
	t.Logf("work-start law, constructors (#586): %d go statements in New* functions", len(found))
}
