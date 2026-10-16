package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #342 (audit 2026-10-12), Art 50 disclosure design 2026-07-28 §11.4.5.
//
// Six delivery paths — then nine more found while taking this very inventory —
// sent agent-authored content to people by calling a transport directly, past
// the ChannelReceiver chokepoint, with no EU AI Act Art 50 notice. Each fix
// routes the site through one disclosure primitive. This law is what stops the
// sixteenth.
//
// It parses every non-test Go file of the module and lists every OUTBOUND SEND
// SITE, keyed "<package dir> <Recv.>Func":
//
//   - any call of a method named Send, SendFile or SendArtifactFile;
//   - in internal/telegram, any call of a Bot send primitive (send*/Send*);
//   - in internal/forge/github and internal/github, any map literal with a
//     "body" key (the GitHub comment/review/PR payloads).
//
// Every function holding a site must be in outboundSites, classified:
//
//   - disclosed: every send call in it is preceded (by source position) by a call
//     to a disclosure primitive — checked here, not trusted from the table;
//   - exempt: deterministic text, with the reason written down;
//   - transport: the implementation under the disclosure path, with the caller
//     that discloses named in the reason.
//
// Each row pins the NUMBER of sites in the function, so a second, bare send
// added beside a disclosed one fails and reaches a reviewer (review r1 F2). Every
// direct call of an error-returning primitive must be fail-closed gated: its
// error is tested by the next statement, whose body ends in return / continue /
// break, or returned directly (review r2 M2). The `authored` argument of each
// SendDisclosed call and Outbound{} literal must be a literal bool matching the
// row (review r2 L1). And every non-test ChannelReceiver literal must set
// Disclosure (review r2 M1).
//
// What it does NOT cover, said here so a clean run is not read as more than it
// is: a send through a method of another name; a raw HTTP call outside these
// packages; whether an exempt row's text is in fact deterministic (a reviewer's
// judgement, recorded in the reason); whether a preceding disclosure call
// actually feeds the following send (position is checked, data flow is not —
// the count pin forces review of any change); and content a person PULLS (UI and
// companion downloads, HTTP API responses), which never passes a send call.

type outboundClass string

const (
	classDisclosed outboundClass = "disclosed"
	classExempt    outboundClass = "exempt"
	classTransport outboundClass = "transport"
)

type outboundRow struct {
	class    outboundClass
	sites    int
	authored string // "true" / "false" for SendDisclosed / Outbound{} in the function; "" = none expected
	reason   string
}

// disclosurePrimitive is one recognised way of applying the Art 50 notice.
// returnsErr is checked against the primitive's real signature; every
// error-returning primitive is fail-closed gated unless ungatedWhy says why not
// (round 3 F4: the gate set cannot drift from the primitive set).
type disclosurePrimitive struct {
	returnsErr bool
	ungatedWhy string
}

var disclosurePrimitives = map[string]disclosurePrimitive{
	"Disclose":            {returnsErr: true},
	"discloseChat":        {returnsErr: true},
	"discloseForumThread": {returnsErr: true},
	"serveDisclosure":     {returnsErr: true},
	"SendDisclosed":       {returnsErr: true, ungatedWhy: "owns its conditional send: the content is never sent when the notice fails"},
	"WithFooter":          {returnsErr: false},
	"postprocess":         {returnsErr: false},
}

const (
	whyTelegramCommand = "command/status reply on a per-session chat composed from fixed templates and stored state; LLM turns go through ChannelReceiver"
	whyTelegramAlert   = "operator alert built from system-controlled fields only (no model output)"
	whyTGTransport     = "Bot API primitive under the disclosure path; every AI-content caller discloses first (discloseChat / discloseForumThread)"
	whyFileSender      = "delivers through a FileSender / ReminderFileSender; every implementation discloses (emailFileSender, slackFileSender, Bot.SendDocumentReader)"
	whyForgeBody       = "forge provider: the body arrives already carrying the publication notice (aidisclosure.WithFooter in forge.open_change_request, forge.post_review, forgeci ingest)"
)

// outboundSites is the Art 50 outbound inventory (design §11.4.4). Keyed
// "<package dir> <Recv.>Func"; the count is the number of send sites in it.
var outboundSites = map[string]outboundRow{
	// --- the shared primitive and the conversational chokepoint ---
	"internal/conversation SendDisclosed":                 {classDisclosed, 2, "?", "the primitive itself: authored is its parameter, not a literal"},
	"internal/dispatcher ChannelReceiver.serveDisclosure": {classDisclosed, 1, "false", ""},
	"internal/dispatcher ChannelReceiver.sendReply":       {classTransport, 1, "", "the receiver's reply send: callers pass postprocess output (per-message footer, §2.5); per-session chats were served by serveDisclosure before dispatch"},
	"internal/dispatcher ToolExecutor.sendArtifact":       {classTransport, 2, "", whyFileSender},
	"internal/dispatcher deliverRenderedFiles":            {classTransport, 1, "", whyFileSender},
	"internal/email Channel.sendOutbound":                 {classTransport, 1, "", "SMTP transport under email Channel.Send/SendFile; disclosed by the receiver footer or SendDisclosed / emailFileSender"},

	// --- GitHub #342 rows 4, 9, 10, 17 and the inventory extras (§11.4.1) ---
	"internal/forge/github Provider.CloseIssue":                      {classDisclosed, 1, "", ""},
	"internal/forge/github Provider.OpenChangeRequest":               {classTransport, 1, "", whyForgeBody},
	"internal/forge/github Provider.PostReview":                      {classTransport, 2, "", whyForgeBody},
	"internal/forge/github Provider.PostComment":                     {classTransport, 1, "", whyForgeBody},
	"internal/service emailSenderAdapter.SendEmail":                  {classDisclosed, 1, "true", ""},
	"internal/service emailFileSender.SendArtifactFile":              {classDisclosed, 1, "true", ""},
	"internal/service slackFileSender.SendArtifactFile":              {classDisclosed, 2, "true", ""},
	"internal/service chatCompletionNotifier.NotifyTaskCompleted":    {classDisclosed, 1, "true", ""},
	"internal/reminders CompletionNotifier.NotifyTaskCompleted":      {classDisclosed, 1, "true", ""},
	"internal/reminders CompletionNotifier.deliverOutputArtifacts":   {classTransport, 1, "", whyFileSender},
	"internal/reminders Runner.deliver":                              {classDisclosed, 1, "true", ""},
	"internal/narrator Narrator.pushChatMilestone":                   {classDisclosed, 1, "true", ""},
	"internal/steering Notifier.NotifySteeringRequired":              {classDisclosed, 1, "true", ""},
	"internal/ui Server.DeliverableSend":                             {classDisclosed, 1, "true", ""},
	"internal/slack Channel.sendConfigAssistReply":                   {classDisclosed, 1, "false", ""},
	"internal/steering OperatorAlertNotifier.NotifySteeringRequired": {classExempt, 1, "", "ownerless-task alert: fmt template of task id, project and state (composeText), no model output"},
	"internal/steering OperatorAlertNotifier.NotifyOperator":         {classExempt, 1, "", "cluster-monitor operator alert: subject/body built by the monitor from health state, no model output"},

	// --- Telegram: rows 13 and 14 and the inventory extras ---
	"internal/telegram Bot.NotifyTaskCompleted":             {classDisclosed, 4, "", ""},
	"internal/telegram Bot.SendDocument":                    {classDisclosed, 1, "", ""},
	"internal/telegram Bot.SendDocumentReader":              {classDisclosed, 1, "", ""},
	"internal/telegram Bot.discloseChat":                    {classDisclosed, 1, "false", ""},
	"internal/telegram Bot.discloseForumThread":             {classDisclosed, 1, "false", ""},
	"internal/telegram Bot.ensureTaskThread":                {classDisclosed, 1, "", ""},
	"internal/telegram Bot.notifyForumThread":               {classDisclosed, 2, "", ""},
	"internal/telegram Bot.sendArtifactsToForum":            {classDisclosed, 2, "", ""},
	"internal/telegram Bot.handleSummarize":                 {classDisclosed, 6, "", ""},
	"internal/telegram Bot.runConfigAssistant":              {classDisclosed, 1, "", ""},
	"internal/telegram Bot.sendSkillReviewDigest":           {classDisclosed, 1, "", ""},
	"internal/telegram Bot.sendArtifactsToWatchers":         {classTransport, 1, "", "uploads through Bot.SendDocument, which discloses (row above)"},
	"internal/telegram Bot.periodicSaveLoop":                {classTransport, 1, "", "calls sendSkillReviewDigest, which discloses (row above)"},
	"internal/telegram Bot.sendDocument":                    {classTransport, 1, "", whyTGTransport},
	"internal/telegram Bot.sendDocumentBytesToForum":        {classTransport, 1, "", whyTGTransport},
	"internal/telegram Bot.sendDocumentToForum":             {classTransport, 1, "", whyTGTransport},
	"internal/telegram Bot.sendMessage":                     {classTransport, 1, "", whyTGTransport},
	"internal/telegram Bot.sendMessageWithMarkup":           {classTransport, 1, "", whyTGTransport},
	"internal/telegram Bot.sendVoiceReply":                  {classTransport, 1, "", "voice reply under telegram Channel.Send — the receiver path, disclosed per session by serveDisclosure"},
	"internal/telegram Channel.Send":                        {classTransport, 3, "", "the conversation.Channel implementation: called by ChannelReceiver and SendDisclosed, which disclose"},
	"internal/telegram Channel.StreamingSend":               {classTransport, 1, "", "receiver streaming path: the chat was served by serveDisclosure before dispatch"},
	"internal/telegram artifactChatSender.SendArtifactFile": {classTransport, 1, "", "send_artifact in a receiver turn; delegates to Bot.SendDocumentReader, which discloses"},
	"internal/telegram Bot.HandleMessage":                   {classExempt, 43, "", whyTelegramCommand},
	"internal/telegram Bot.handleAutopilot":                 {classExempt, 8, "", whyTelegramCommand},
	"internal/telegram Bot.handleLinkClaim":                 {classExempt, 5, "", whyTelegramCommand},
	"internal/telegram Bot.handleLinkCommand":               {classExempt, 3, "", whyTelegramCommand},
	"internal/telegram Bot.handleLinkIssue":                 {classExempt, 1, "", whyTelegramCommand},
	"internal/telegram Bot.tryAccountLinkCode":              {classExempt, 4, "", whyTelegramCommand},
	"internal/telegram Bot.handleProjectCallback":           {classExempt, 1, "", whyTelegramCommand},
	"internal/telegram Bot.sendErrorMessage":                {classExempt, 1, "", whyTelegramCommand},
	"internal/telegram Bot.sendProjectPicker":               {classExempt, 1, "", whyTelegramCommand},
	"internal/telegram handleProject":                       {classExempt, 1, "", whyTelegramCommand},
	"internal/telegram handleStart":                         {classExempt, 2, "", whyTelegramCommand},
	"internal/telegram Bot.routeReplyToTask":                {classExempt, 7, "", "acknowledgement / denial of an operator reply: fixed strings plus the error text"},
	"internal/telegram Bot.notifyAfterPanic":                {classExempt, 1, "", "fixed fault message on the config-assistant panic path"},
	"internal/telegram Bot.NotifyBrokerActionsPending":      {classExempt, 1, "", whyTelegramAlert + " (buildBrokerActionCaption)"},
	"internal/telegram Bot.NotifyWebWritePending":           {classExempt, 3, "", whyTelegramAlert + " (buildWebWriteCaption); the screenshot is the third-party page awaiting approval — the agent-filled form is row 26, an open operator question (§11.4.7)"},
	"internal/telegram Bot.NotifyScraperBlock":              {classExempt, 3, "", whyTelegramAlert + " (scraper block: host, reason, screenshot of the blocking page)"},
	"internal/telegram Bot.NotifyBudgetBreach":              {classExempt, 1, "", whyTelegramAlert + " (budget figures)"},
	"internal/telegram Bot.NotifyEffectiveCostDrift":        {classExempt, 1, "", whyTelegramAlert + " (cost figures)"},
	"internal/telegram Bot.flushFillNotification":           {classExempt, 1, "", whyTelegramAlert + " (broker fill figures)"},
}

var telegramSendPrimitive = regexp.MustCompile(`^(send[A-Z]\w*|sendMessage|sendDocument|Send[A-Z]\w*)$`)

type outboundSite struct {
	key, file string
	line      int
	pos       token.Pos
	self      bool // a SendDisclosed call: discloses by construction
}

type outboundFunc struct {
	key        string
	file       string
	sites      []outboundSite
	primitives []token.Pos
	authored   []string // literal values seen; "?" = not a literal
	gateFaults []string
}

func TestArt50Law_EveryOutboundSendGoesThroughTheDisclosurePath(t *testing.T) {
	root := moduleRoot(t)
	files := 0
	funcs := map[string]*outboundFunc{}
	var receiverFaults []string
	primitiveSigs := map[string]bool{} // primitive name -> returns error (from its declaration)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name(), "testdata", "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		files++
		scanOutboundFile(t, path, rel, funcs, &receiverFaults, primitiveSigs)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if files < 100 {
		t.Fatalf("examined only %d Go files under %s: the scan is not looking at the module", files, root)
	}

	// Primitive table is honest about which primitives can fail.
	for name, p := range disclosurePrimitives {
		got, found := primitiveSigs[name]
		if !found {
			t.Errorf("disclosure primitive %q is no longer declared anywhere: update the table and the design together", name)
			continue
		}
		if got != p.returnsErr {
			t.Errorf("disclosure primitive %q: table says returnsErr=%v, declaration says %v", name, p.returnsErr, got)
		}
	}

	keys := make([]string, 0, len(funcs))
	for k := range funcs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	counts := map[outboundClass]int{}
	sites := 0
	for _, k := range keys {
		f := funcs[k]
		for _, g := range f.gateFaults {
			t.Errorf("%s: %s", f.file, g)
		}
		if len(f.sites) == 0 {
			continue
		}
		sites += len(f.sites)
		row, ok := outboundSites[k]
		if !ok {
			t.Errorf("%s:%d (%s) sends to a person (%d site(s)) and is not in the Art 50 outbound inventory. "+
				"Route it through conversation.SendDisclosed / aidisclosure (design 2026-07-28 §11.4, GitHub #342), "+
				"or add a row here with an exempt reason a reviewer can check",
				f.file, f.sites[0].line, k, len(f.sites))
			continue
		}
		counts[row.class] += len(f.sites)
		if row.sites != len(f.sites) {
			t.Errorf("%s: %d send site(s), the inventory pins %d — a send was added or removed; re-review its disclosure and update the row",
				k, len(f.sites), row.sites)
		}
		if row.class != classDisclosed && strings.TrimSpace(row.reason) == "" {
			t.Errorf("%s: %s row needs a reason", k, row.class)
		}
		if row.class == classDisclosed {
			for _, s := range f.sites {
				if !s.self && !anyBefore(f.primitives, s.pos) {
					t.Errorf("%s:%d (%s): send is not preceded by a disclosure primitive in its function", s.file, s.line, k)
				}
			}
		}
		for _, a := range f.authored {
			if a != row.authored {
				t.Errorf("%s: authored argument %q, the inventory expects %q (a literal true/false; review r2 L1)", k, a, row.authored)
			}
		}
	}
	for k, row := range outboundSites {
		if f, ok := funcs[k]; !ok || len(f.sites) == 0 {
			t.Errorf("inventory row %q (%s) matches no send site: stale — remove it, and check the design", k, row.class)
		}
	}
	for _, rf := range receiverFaults {
		t.Error(rf)
	}

	t.Logf("Art 50 outbound law: examined %d non-test Go files; %d send site(s) in %d function(s): "+
		"%d disclosed, %d exempt, %d transport",
		files, sites, len(outboundSites), counts[classDisclosed], counts[classExempt], counts[classTransport])
}

func anyBefore(ps []token.Pos, p token.Pos) bool {
	for _, x := range ps {
		if x < p {
			return true
		}
	}
	return false
}

func scanOutboundFile(t *testing.T, path, rel string, funcs map[string]*outboundFunc, receiverFaults *[]string, primitiveSigs map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	inTelegram := dir == "internal/telegram"
	inGitHub := dir == "internal/forge/github" || dir == "internal/github"

	// ChannelReceiver literals anywhere in the file (package-level vars too).
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isChannelReceiverType(cl.Type) {
			return true
		}
		if !hasKey(cl, "Disclosure") {
			*receiverFaults = append(*receiverFaults, fmt.Sprintf(
				"%s:%d: ChannelReceiver literal without Disclosure — the chokepoint would serve no Art 50 notice on this channel (review r2 M1)",
				rel, fset.Position(cl.Pos()).Line))
		}
		return true
	})

	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if _, isPrim := disclosurePrimitives[fd.Name.Name]; isPrim {
			primitiveSigs[fd.Name.Name] = returnsError(fd)
		}
		key := dir + " " + funcName(fd)
		of := funcs[key]
		if of == nil {
			of = &outboundFunc{key: key, file: rel}
			funcs[key] = of
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				name := calleeName(x)
				if _, isPrim := disclosurePrimitives[name]; isPrim {
					of.primitives = append(of.primitives, x.Pos())
				}
				if name == "SendDisclosed" && len(x.Args) == 5 {
					of.authored = append(of.authored, boolLiteral(x.Args[4]))
					// A SendDisclosed call is a send site that discloses by
					// construction; counted so it is in the denominator.
					of.sites = append(of.sites, outboundSite{key: key, file: rel, line: fset.Position(x.Pos()).Line, pos: x.Pos(), self: true})
				}
				if _, ok := x.Fun.(*ast.SelectorExpr); ok {
					isSend := name == "Send" || name == "SendFile" || name == "SendArtifactFile"
					if inTelegram && telegramSendPrimitive.MatchString(name) {
						isSend = true
					}
					if isSend {
						of.sites = append(of.sites, outboundSite{key: key, file: rel, line: fset.Position(x.Pos()).Line, pos: x.Pos()})
					}
				}
			case *ast.CompositeLit:
				if isOutboundLiteral(x.Type) {
					of.authored = append(of.authored, outboundAuthored(x))
				}
				if inGitHub && isMapLit(x) && hasStringKey(x, "body") {
					of.sites = append(of.sites, outboundSite{key: key, file: rel, line: fset.Position(x.Pos()).Line, pos: x.Pos()})
				}
			case *ast.BlockStmt:
				checkGates(fset, x.List, of)
			case *ast.CaseClause:
				checkGates(fset, x.Body, of)
			case *ast.CommClause:
				checkGates(fset, x.Body, of)
			}
			return true
		})
	}
}

// checkGates enforces the fail-closed shape on every direct call of a gated
// primitive among stmts.
func checkGates(fset *token.FileSet, stmts []ast.Stmt, of *outboundFunc) {
	for i, st := range stmts {
		switch s := st.(type) {
		case *ast.AssignStmt:
			if !containsGatedCall(s.Rhs) {
				continue
			}
			errName := lastIdent(s.Lhs)
			if errName == "" || errName == "_" {
				of.gateFaults = append(of.gateFaults, fmt.Sprintf("line %d: a disclosure primitive's error is discarded — the content would go out undisclosed when the notice fails (review r2 M2)", fset.Position(s.Pos()).Line))
				continue
			}
			if i+1 >= len(stmts) || !gatesOn(stmts[i+1], errName) {
				of.gateFaults = append(of.gateFaults, fmt.Sprintf("line %d: the disclosure primitive's error %q must be tested by the very next statement, ending in return/continue/break (review r2 M2)", fset.Position(s.Pos()).Line, errName))
			}
		case *ast.IfStmt:
			if as, ok := s.Init.(*ast.AssignStmt); ok && containsGatedCall(as.Rhs) {
				errName := lastIdent(as.Lhs)
				if errName == "" || errName == "_" || !condTestsNonNil(s.Cond, errName) || !endsInExit(s.Body) {
					of.gateFaults = append(of.gateFaults, fmt.Sprintf("line %d: a disclosure primitive in an if-init must be gated as `if err := …; err != nil { …return/continue/break }` (review r2 M2)", fset.Position(s.Pos()).Line))
				}
			}
		case *ast.ExprStmt:
			if containsGatedCall([]ast.Expr{s.X}) {
				of.gateFaults = append(of.gateFaults, fmt.Sprintf("line %d: a disclosure primitive's result is ignored (review r2 M2)", fset.Position(s.Pos()).Line))
			}
		}
	}
}

func containsGatedCall(exprs []ast.Expr) bool {
	for _, e := range exprs {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			continue
		}
		p, isPrim := disclosurePrimitives[calleeName(call)]
		if isPrim && p.returnsErr && p.ungatedWhy == "" {
			return true
		}
	}
	return false
}

func gatesOn(st ast.Stmt, errName string) bool {
	switch s := st.(type) {
	case *ast.IfStmt:
		return s.Init == nil && condTestsNonNil(s.Cond, errName) && endsInExit(s.Body)
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			if id, ok := r.(*ast.Ident); ok && id.Name == errName {
				return true
			}
		}
	}
	return false
}

func condTestsNonNil(cond ast.Expr, errName string) bool {
	be, ok := cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return false
	}
	id, ok := be.X.(*ast.Ident)
	nilID, ok2 := be.Y.(*ast.Ident)
	return ok && ok2 && id.Name == errName && nilID.Name == "nil"
}

func endsInExit(b *ast.BlockStmt) bool {
	if b == nil || len(b.List) == 0 {
		return false
	}
	switch s := b.List[len(b.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return s.Tok == token.CONTINUE || s.Tok == token.BREAK
	}
	return false
}

func lastIdent(lhs []ast.Expr) string {
	if len(lhs) == 0 {
		return ""
	}
	if id, ok := lhs[len(lhs)-1].(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func returnsError(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
		return false
	}
	last := fd.Type.Results.List[len(fd.Type.Results.List)-1].Type
	id, ok := last.(*ast.Ident)
	return ok && id.Name == "error"
}

func isChannelReceiverType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "ChannelReceiver"
	case *ast.Ident:
		return x.Name == "ChannelReceiver"
	}
	return false
}

func isOutboundLiteral(e ast.Expr) bool {
	if x, ok := e.(*ast.SelectorExpr); ok {
		if pkg, ok := x.X.(*ast.Ident); ok {
			return pkg.Name == "aidisclosure" && x.Sel.Name == "Outbound"
		}
	}
	return false
}

func outboundAuthored(cl *ast.CompositeLit) string {
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Authored" {
			return boolLiteral(kv.Value)
		}
	}
	return "false" // omitted = the zero value, a literal false
}

func boolLiteral(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
		return id.Name
	}
	return "?"
}

func hasKey(cl *ast.CompositeLit, key string) bool {
	for _, el := range cl.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key {
				return true
			}
		}
	}
	return false
}

func isMapLit(cl *ast.CompositeLit) bool {
	_, ok := cl.Type.(*ast.MapType)
	return ok
}

func hasStringKey(cl *ast.CompositeLit, key string) bool {
	for _, el := range cl.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Value == `"`+key+`"` {
				return true
			}
		}
	}
	return false
}

// TestArt50Law_CatchesAPlantedBypass proves the scanner sees what it claims: a
// bare Send, a second send beside a disclosed one, a discarded primitive error,
// and an unwired receiver are all reported.
func TestArt50Law_CatchesAPlantedBypass(t *testing.T) {
	dir := t.TempDir()
	src := `package p
import "context"
type ch interface{ Send(context.Context, string) (string, error) }
type svc struct{}
func (svc) Disclose(context.Context, int) (string, error) { return "", nil }
type N struct{ c ch; d svc }
func (n N) bare(ctx context.Context) { _, _ = n.c.Send(ctx, "x") }
func (n N) twice(ctx context.Context) error {
	body, err := n.d.Disclose(ctx, 0)
	if err != nil { return err }
	_, _ = n.c.Send(ctx, body)
	_, _ = n.c.Send(ctx, "second")
	return nil
}
func (n N) ignored(ctx context.Context) {
	_, _ = n.d.Disclose(ctx, 0)
	_, _ = n.c.Send(ctx, "x")
}
var r = ChannelReceiver{Agent: nil}
type ChannelReceiver struct{ Agent any; Disclosure any }
`
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*outboundFunc{}
	var recv []string
	sigs := map[string]bool{}
	scanOutboundFile(t, path, "internal/p/p.go", funcs, &recv, sigs)

	if f := funcs["internal/p N.bare"]; f == nil || len(f.sites) != 1 || len(f.primitives) != 0 {
		t.Fatalf("bare send not seen as an undisclosed site: %+v", f)
	}
	if f := funcs["internal/p N.twice"]; f == nil || len(f.sites) != 2 {
		t.Fatalf("the second send beside a disclosed one must count (pin = 2): %+v", f)
	}
	if f := funcs["internal/p N.ignored"]; f == nil || len(f.gateFaults) != 1 {
		t.Fatalf("a discarded primitive error must be a gate fault: %+v", f)
	}
	if len(recv) != 1 {
		t.Fatalf("an unwired ChannelReceiver literal must be reported, got %v", recv)
	}
}
