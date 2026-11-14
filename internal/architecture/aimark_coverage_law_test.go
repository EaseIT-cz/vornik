package architecture

// GitHub #117, EU AI Act Art 50(2), Art 50 design §14.8 B commit 6: every
// place that delivers generated output must mark it (sign it, or record it
// in the ledger where no carrier exists), or be listed here as exempt with
// the reason. This is what stops an unmarked fifteenth path.
//
// Delivery points are FOUND, not only listed, in the packages that talk to
// an outside service: a function whose body names a delivery API (Telegram
// /sendMessage, /sendVoice, /sendDocument, /sendPhoto, /editMessageText;
// Slack /chat.postMessage, /chat.update, /files.completeUploadExternal), calls
// the email renderer, or builds a GitHub "body" payload. Each must be a row.
// The HTTP response surfaces have no common shape, so they are named rows
// that must exist and must mark. Rows 20, 21 and 24 (machine to machine,
// guidelines para 68) are named exclusions that must still exist, so a
// rename cannot quietly drop the record of the decision.
//
// What it does NOT cover, said so a clean run is not read as more: a delivery
// API outside these packages and named rows; whether a marking call really
// covers the bytes sent (the per-surface tests check that); and output people
// pull through an API that is not a named row. Marking is matched by the
// called function's name, so a call through a function variable or an
// interface value is not seen, and a function merely NAMED like a marking
// helper would count: the per-surface tests are the authority on whether a
// path really marks.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// markingCalls are the functions that sign or record a mark. The CE parity
// gate's MARKING_SYMBOLS (scripts/check-ce-compliance-parity.sh) must name
// each one; its selftest fails when one is missing (Art 50 design §14.11).
var markingCalls = map[string]bool{
	"markOutput": true, "ollamaMark": true, "markArtifactDownload": true, "markDownload": true,
	"markChatReplies": true, "markBody": true, "withMark": true, "markText": true,
	"recordAIText": true, "recordAIAudio": true, "markSource": true, "recordRendered": true,
	"recordHarvestedOutput": true,
}

// deliveryTriggers find delivery points by what they call or name.
var deliveryTriggers = map[string][]string{
	"internal/telegram":     {"/sendMessage", "/sendVoice", "/sendDocument", "/sendPhoto", "/editMessageText"},
	"internal/slack":        {"/chat.postMessage", "/chat.update", "/files.completeUploadExternal"},
	"internal/email":        {"RenderRFC5322("},
	"internal/forge/github": {`"body"`},
	"internal/github":       {`"body"`},
}

type markRow struct {
	marked bool   // must call a marking function
	reason string // why it is exempt (or what it marks)
}

// markRows: every found delivery point and every named surface, keyed
// "<dir> <Recv.>Func".
var markRows = map[string]markRow{
	// Found delivery points that mark.
	"internal/email Channel.sendOutbound":              {true, "rows 8-10: X-AI-Generated over the body"},
	"internal/forge/github Provider.OpenChangeRequest": {true, "row 1: PR body"},
	"internal/forge/github Provider.PostReview":        {true, "row 2: review body and its fallback comment"},
	"internal/forge/github Provider.PostComment":       {true, "row 4: comment"},
	"internal/forge/github Provider.CloseIssue":        {true, "row 4: duplicate-close comment"},
	"internal/github Channel.sendIssueComment":         {true, "row 7: conversation channel reply"},
	"internal/slack Channel.sendChatPostMessage":       {true, "row 15: message metadata"},
	"internal/telegram Bot.doSendMessage":              {true, "rows 11, 13: ledger row"},
	"internal/telegram Bot.sendMessageGetID":           {true, "rows 11, 13: ledger row"},
	"internal/telegram Bot.sendForumMessage":           {true, "rows 11, 13: ledger row"},
	"internal/telegram Bot.sendVoice":                  {true, "row 12: ledger row over the audio"},
	// Found delivery points that are exempt.
	"internal/forge/github Provider.issueComments":   {false, "reads comments (GET); sends nothing"},
	"internal/slack Channel.completeExternalUpload":  {false, "file-upload transport: a voice reply is recorded by its caller sendVoiceReply; files are marked at creation (render_document) or recorded at harvest"},
	"internal/slack Channel.postPlaceholder":         {false, "a status placeholder, not generated content; the answer goes through sendChatPostMessage"},
	"internal/slack Channel.updatePlaceholder":       {false, "rewrites the status placeholder; not generated content"},
	"internal/telegram Bot.editMessageTextAndMarkup": {false, "a streamed reply is recorded once complete (telegramStream.Close); the steering callback edit is a fixed label"},
	"internal/telegram Bot.sendDocumentFromReader":   {false, "a file: marked at creation (render_document) or recorded at harvest (executor)"},
	"internal/telegram Bot.sendDocumentToForumOnce":  {false, "a file: marked at creation or recorded at harvest"},
	"internal/telegram Bot.sendPhotoBytes":           {false, "an image file: recorded at harvest when an agent produced it"},
	// Named surfaces (no common shape to find them by).
	"internal/api Server.ChatCompletions":             {true, "row 18"},
	"internal/api Server.OllamaChat":                  {true, "row 19"},
	"internal/api Server.OllamaGenerate":              {true, "row 19"},
	"internal/api Server.companionArtifactDownload":   {true, "row 22"},
	"internal/ui Server.ArtifactDownload":             {true, "row 23"},
	"internal/ui Server.runChatTurn":                  {true, "row 16: a web chat reply is marked when its background turn ends; ChatTurnPoll and ChatPostMessage deliver it with the turn's mark headers (web-chat-design.md §2.9)"},
	"internal/dispatcher ToolExecutor.renderDocument": {true, "row 27"},
	"internal/executor Executor.harvestOne":           {true, "every agent-produced file (rows 22, 23, 28)"},
	"internal/telegram telegramStream.Close":          {true, "rows 11, 13: a streamed reply's final text"},
	"internal/slack Channel.sendVoiceReply":           {true, "row 15 voice: ledger row over the audio"},
}

// excludedRows are delivered machine to machine (guidelines para 68) and not
// marked; each must still exist.
var excludedRows = map[string]string{
	"internal/api Server.companionToolResult":      "row 20: companion MCP result, to another AI agent",
	"internal/api Server.companionBrokerResult":    "row 21: broker egress document, to another AI agent",
	"internal/conversation/a2a emitAnswerArtifact": "row 24: A2A answer, to another AI agent",
}

type foundFunc struct {
	calls    map[string]bool
	triggers bool
}

func scanMarkFuncs(t *testing.T, root, dir string, pats []string) map[string]foundFunc {
	t.Helper()
	out := map[string]foundFunc{}
	files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) //nolint:gosec // the module's own sources
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := dir + " " + funcKey(fd)
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
			ff := foundFunc{calls: map[string]bool{}}
			for _, p := range pats {
				if strings.Contains(body, p) && (p != "RenderRFC5322(" || fd.Name.Name != "RenderRFC5322") {
					ff.triggers = true
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					switch fn := c.Fun.(type) {
					case *ast.Ident:
						ff.calls[fn.Name] = true
					case *ast.SelectorExpr:
						ff.calls[fn.Sel.Name] = true
					}
				}
				return true
			})
			out[key] = ff
		}
	}
	return out
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func TestAIMarkCoverageLaw(t *testing.T) {
	root := moduleRoot(t)
	dirs := map[string][]string{}
	for d, p := range deliveryTriggers {
		dirs[d] = p
	}
	for key := range markRows {
		d := strings.SplitN(key, " ", 2)[0]
		if _, ok := dirs[d]; !ok {
			dirs[d] = nil
		}
	}
	for key := range excludedRows {
		d := strings.SplitN(key, " ", 2)[0]
		if _, ok := dirs[d]; !ok {
			dirs[d] = nil
		}
	}
	all := map[string]foundFunc{}
	for d, p := range dirs {
		for k, v := range scanMarkFuncs(t, root, d, p) {
			all[k] = v
		}
	}
	var found, problems []string
	for key, ff := range all {
		if ff.triggers {
			found = append(found, key)
			if _, ok := markRows[key]; !ok {
				problems = append(problems, key+": a delivery point with no row: mark it, or add an exempt row with the reason")
			}
		}
	}
	for key, row := range markRows {
		ff, ok := all[key]
		switch {
		case !ok:
			problems = append(problems, key+": row names a function that no longer exists")
		case row.reason == "":
			problems = append(problems, key+": row without a reason")
		case row.marked && !callsMarking(ff):
			problems = append(problems, key+": must mark ("+row.reason+") but calls no marking function")
		}
	}
	for key := range excludedRows {
		if _, ok := all[key]; !ok {
			problems = append(problems, key+": excluded row names a function that no longer exists; record the decision on its new name")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	// The denominator (CLAUDE.md §4): what was examined, not only what failed.
	t.Logf("examined %d functions in %d packages; %d delivery points found; %d rows; %d named exclusions",
		len(all), len(dirs), len(found), len(markRows), len(excludedRows))
	if len(found) == 0 {
		t.Fatal("no delivery point found: the triggers no longer match the code, so this law examined nothing")
	}
}

func callsMarking(ff foundFunc) bool {
	for c := range ff.calls {
		if markingCalls[c] {
			return true
		}
	}
	return false
}
