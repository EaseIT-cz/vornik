package architecture

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// SQLite stores timestamps as TEXT and compares them as strings, so every
// timestamp a SQLite repository writes or binds must be in ONE fixed-width
// form (sqliteTime: 2006-01-02T15:04:05.000000000Z). Three other forms were in
// use and none orders correctly against the others, or (RFC3339Nano) against
// itself below one second. Design:
// https://docs.vornik.io (D3).
//
// The law, on non-test files of internal/persistence/sqlite — the package
// every SQLite read and write goes through:
//   - no time.Time / *time.Time bound to a query method, appended to an
//     argument slice, or inside a []any{...} literal spread into a query
//     method (a []time.Time cannot be spread into a []any, so there is no
//     spread case to check);
//   - no Format with an RFC3339 layout, or any layout literal containing
//     15:04:05, outside helpers.go (a date-only layout is fixed-width);
//   - no CURRENT_TIMESTAMP, and no SQLite date function called with 'now'
//     or "now", in a string outside schema.go (whose DDL declares the
//     defaults);
//   - no INSERT, in any file including schema.go, into a table with a
//     DEFAULT CURRENT_TIMESTAMP column that does not name that column (the
//     default would store the space form).
//
// The package must type-check: a type error fails the law rather than being
// skipped, because skipped files are unchecked files.

var sqliteQueryMethods = map[string]bool{
	"ExecContext": true, "QueryContext": true, "QueryRowContext": true,
	"Exec": true, "Query": true, "QueryRow": true,
}

var sqliteNowCall = regexp.MustCompile(`(?i)(datetime|date|time|strftime|julianday|unixepoch)\s*\([^)]*['"]now['"]`)

func TestSQLiteTimestampsAreWrittenInOneForm(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
		packages.NeedTypes | packages.NeedTypesInfo}
	pkgs, err := packages.Load(cfg, "vornik.io/vornik/internal/persistence/sqlite")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var violations []string
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			t.Fatalf("type-check %s: %v", pkg.PkgPath, e)
		}
		var defaults map[string][]string
		for _, f := range pkg.GoFiles {
			if filepath.Base(f) == "schema.go" {
				src, rerr := os.ReadFile(f)
				if rerr != nil {
					t.Fatal(rerr)
				}
				defaults = defaultedTimestampColumns(string(src))
			}
		}
		// The denominator: a parse that finds fewer defaulted columns than
		// the schema has would quietly exempt a table's inserts.
		n := 0
		for _, cols := range defaults {
			n += len(cols)
		}
		if n != defaultedTimestampColumnCount {
			t.Fatalf("found %d DEFAULT CURRENT_TIMESTAMP columns in schema.go, want %d: update the count if the schema changed, or fix the parse (%v)", n, defaultedTimestampColumnCount, defaults)
		}
		for _, file := range pkg.Syntax {
			name := filepath.Base(pkg.Fset.Position(file.Pos()).Filename)
			violations = append(violations, sqliteTimeViolations(pkg.Fset, pkg.TypesInfo, file, name, defaults)...)
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// defaultedTimestampColumnCount is how many DEFAULT CURRENT_TIMESTAMP
// columns schema.go declares (design D3).
const defaultedTimestampColumnCount = 4

// defaultedTimestampColumns maps each table to its DEFAULT CURRENT_TIMESTAMP
// columns, read from the schema DDL.
func defaultedTimestampColumns(schema string) map[string][]string {
	out := map[string][]string{}
	table := ""
	tableRE := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	colRE := regexp.MustCompile(`(?i)^\s*(\w+)\s+\w+.*DEFAULT\s+\(?\s*CURRENT_TIMESTAMP`)
	for _, line := range strings.Split(schema, "\n") {
		if m := tableRE.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if m := colRE.FindStringSubmatch(line); m != nil && table != "" {
			out[table] = append(out[table], m[1])
		}
	}
	return out
}

func sqliteTimeViolations(fset *token.FileSet, info *types.Info, file *ast.File, name string, defaults map[string][]string) []string {
	var out []string
	at := func(n ast.Node) string { return name + ":" + strconv.Itoa(fset.Position(n.Pos()).Line) }
	typeOf := func(e ast.Expr) string {
		if tv, ok := info.Types[e]; ok && tv.Type != nil {
			return tv.Type.String()
		}
		return ""
	}
	isTime := func(e ast.Expr) bool { s := typeOf(e); return s == "time.Time" || s == "*time.Time" }
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, isSel := x.Fun.(*ast.SelectorExpr)
			if isSel && sqliteQueryMethods[sel.Sel.Name] {
				for i, arg := range x.Args {
					if isTime(arg) {
						out = append(out, at(arg)+": time.Time bound to "+sel.Sel.Name+"; pass sqliteTime(...)")
					}
					if lit, ok := arg.(*ast.CompositeLit); ok && x.Ellipsis.IsValid() && i == len(x.Args)-1 {
						for _, el := range lit.Elts {
							if isTime(el) {
								out = append(out, at(el)+": time.Time inside a spread argument literal; pass sqliteTime(...)")
							}
						}
					}
				}
			}
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "append" && len(x.Args) > 1 {
				for _, arg := range x.Args[1:] {
					if isTime(arg) {
						out = append(out, at(arg)+": time.Time appended to a query argument slice; pass sqliteTime(...)")
					}
				}
			}
			if isSel && sel.Sel.Name == "Format" && name != "helpers.go" && len(x.Args) == 1 && isTime(sel.X) {
				// The layout's constant value, however it is spelled:
				// time.RFC3339Nano, a literal, or a local const all resolve
				// here (review-20261001-77ff suggestion 3). A date-only layout
				// is fixed-width and allowed.
				if tv, ok := info.Types[x.Args[0]]; ok && tv.Value != nil && tv.Value.Kind() == constant.String &&
					strings.Contains(constant.StringVal(tv.Value), "15:04:05") {
					out = append(out, at(x)+": Format with a time-of-day layout ("+constant.StringVal(tv.Value)+"); use sqliteTime")
				}
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			// Match the string's content, not the Go literal: an escaped
			// \"now\" must read as "now".
			text, uerr := strconv.Unquote(x.Value)
			if uerr != nil {
				text = x.Value
			}
			if name != "schema.go" && (strings.Contains(strings.ToUpper(text), "CURRENT_TIMESTAMP") || sqliteNowCall.MatchString(text)) {
				out = append(out, at(x)+": SQL-side clock (CURRENT_TIMESTAMP / 'now'); bind sqliteTime(now)")
			}
			for table, cols := range defaults {
				if !regexp.MustCompile(`(?i)INSERT\s+(OR\s+\w+\s+)?INTO\s+` + table + `\b`).MatchString(text) {
					continue
				}
				for _, col := range cols {
					if !regexp.MustCompile(`\b` + col + `\b`).MatchString(text) {
						out = append(out, at(x)+": INSERT INTO "+table+" omits "+col+", so it stores the CURRENT_TIMESTAMP default's space form; supply sqliteTime(now)")
					}
				}
			}
		}
		return true
	})
	return out
}

// The law's own cases: each rule fires on a fixture.
func TestSQLiteTimeLaw_Fixtures(t *testing.T) {
	src := `package fixture
import ("context"; "database/sql"; "time")
func f(db *sql.DB, t time.Time) {
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, "UPDATE x SET a = ?", t)
	_, _ = db.ExecContext(ctx, "UPDATE x SET a = ?", []any{1, t}...)
	args := []any{}
	args = append(args, t)
	_ = t.Format(time.RFC3339Nano)
	_ = t.Format("2006-01-02 15:04:05")
	_ = t.Format("2006-01-02")
	const layout = time.RFC3339
	_ = t.Format(layout)
	_, _ = db.ExecContext(ctx, "UPDATE x SET a = CURRENT_TIMESTAMP")
	_, _ = db.ExecContext(ctx, "UPDATE x SET a = datetime(\"now\")")
	_, _ = db.ExecContext(ctx, "INSERT INTO probe (id) VALUES (?)", 1)
	_, _ = db.ExecContext(ctx, "INSERT INTO probe (id, created_at) VALUES (?, ?)", 1, "x")
	_ = args
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, err := (&types.Config{Importer: importer.Default()}).Check("fixture", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	got := sqliteTimeViolations(fset, info, file, "fixture.go", map[string][]string{"probe": {"created_at"}})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"time.Time bound to ExecContext",
		"time.Time inside a spread argument literal",
		"time.Time appended",
		"time-of-day layout (2006-01-02T15:04:05.999999999Z07:00)",
		"time-of-day layout (2006-01-02 15:04:05)",
		"time-of-day layout (2006-01-02T15:04:05Z07:00)",
		"SQL-side clock",
		"INSERT INTO probe omits created_at",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no violation %q in:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "SQL-side clock") != 2 {
		t.Errorf("want both CURRENT_TIMESTAMP and datetime(\"now\") flagged:\n%s", joined)
	}
	if strings.Count(joined, "INSERT INTO probe omits") != 1 {
		t.Errorf("the insert naming created_at must pass:\n%s", joined)
	}
	if strings.Contains(joined, "fixture.go:11:") {
		t.Errorf("a date-only layout must pass:\n%s", joined)
	}
}

func TestDefaultedTimestampColumns(t *testing.T) {
	got := defaultedTimestampColumns("CREATE TABLE IF NOT EXISTS a (\n    id TEXT,\n    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP\n);\nCREATE TABLE IF NOT EXISTS b (\n    x TEXT\n);\nCREATE TABLE IF NOT EXISTS c (\n    seen_at TEXT DEFAULT (CURRENT_TIMESTAMP)\n);")
	if len(got) != 2 || len(got["a"]) != 1 || got["a"][0] != "created_at" || len(got["c"]) != 1 || got["c"][0] != "seen_at" {
		t.Fatalf("got %v", got)
	}
}
