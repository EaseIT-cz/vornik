package service

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/storage"
)

// audit 2026-10-12: every internal/service test that built a Container with a
// worker-running profile reached runtime.New's real `podman version` probe,
// bounded at a fixed 5 s, and one was killed under parallel `make test` load
// (#127; the likely cause of TestActionCaller_ScansAtSend's one-off failure in
// #126 too). Unit tests now build containers through newUnitTestContainer,
// which replaces the probe and points the manager at a podman that does not
// exist, so no unit test here ever execs the host's podman (runtime LLD,
// amendment 2026-10-12).

// unitTestPodmanPath is where the runtime manager looks for podman in unit
// tests. It does not exist: any podman exec a unit test reaches fails loudly
// instead of touching the host. The basename is podman because the spawn law
// refuses any other program name.
const unitTestPodmanPath = "/nonexistent/vornik-unit-test/podman"

// unitTestRuntimeOptions are the runtime manager options every unit-test
// container gets.
func unitTestRuntimeOptions() []runtime.ManagerOption {
	return []runtime.ManagerOption{
		runtime.WithPodmanPath(unitTestPodmanPath),
		runtime.WithVersionProbe(func(context.Context) ([]byte, error) {
			return []byte(`{"Client":{"APIVersion":"5.0.0","Version":"5.0.0-unit-test"}}`), nil
		}),
	}
}

// unitTestDatabaseOpener is the database open every unit-test container gets
// (#367): the in-memory SQLite database the test configs ask for is a copy of
// sqlitetest's migrated in-memory image, wrapped as storage.Open would wrap
// it, instead of a migrate from empty per boot; any other database (a test
// that boots on a file) keeps storage.Open. initDatabase's own Migrate still
// runs on the copy.
func unitTestDatabaseOpener(t testing.TB) func(context.Context, config.DatabaseConfig) (*storage.Backend, error) {
	return func(_ context.Context, dc config.DatabaseConfig) (*storage.Backend, error) {
		if dc.Driver == "sqlite" && dc.Path == ":memory:" {
			return storage.NewSQLiteBackend(sqlitetest.Memory(t)), nil
		}
		// The fixture law requires a sanctioned context on a test's
		// storage.Open (initDatabase passes context.Background()).
		return storage.Open(sqlitetest.Context(t), dc)
	}
}

// newUnitTestContainer is NewContainer for this package's tests: the same
// construction, with the podman probe replaced and the in-memory database
// copied from the migrated image (unitTestDatabaseOpener). Options the caller
// passes come after these, so they win.
func newUnitTestContainer(t testing.TB, cfg *config.Config, configPath string, opts ...ContainerOption) (*Container, error) {
	t.Helper()
	all := append([]ContainerOption{
		WithRuntimeManagerOptions(unitTestRuntimeOptions()...),
		WithDatabaseOpener(unitTestDatabaseOpener(t)),
	}, opts...)
	return NewContainer(cfg, configPath, all...)
}

// With no podman anywhere on PATH, a worker-profile container still builds:
// nothing in construction execs podman. Before the seam, runtime.New's
// LookPodman (or, with podman installed, its version probe) decided it.
func TestNewUnitTestContainer_NeedsNoPodman(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = "" // all: runs workers, so initScheduler builds the runtime manager
	c, err := newUnitTestContainer(t, cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("a unit-test container must build without podman: %v", err)
	}
	if c.runtimeManager == nil {
		t.Fatal("the worker profile built no runtime manager; the test proved nothing")
	}
	if got := c.runtimeManager.PodmanPath(); got != unitTestPodmanPath {
		t.Fatalf("runtime manager podman = %q, want the non-existent unit-test path", got)
	}
}

// The law: this package's tests build containers only through the helper. A
// direct NewContainer call would bring the real podman probe back.
func TestServiceTestsBuildContainersThroughTheHelper(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if ok && fd.Name.Name == "newUnitTestContainer" && fd.Recv == nil {
				return false // the helper itself
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "NewContainer" || id.Name == "NewContainerWithObservability" || id.Name == "newUnitTestContainer" {
				calls++
			}
			if id.Name == "NewContainer" || id.Name == "NewContainerWithObservability" {
				t.Errorf("%s: %s called directly; use newUnitTestContainer so the test never execs podman (#127)",
					fset.Position(call.Pos()), id.Name)
			}
			return true
		})
	}
	// The denominator: a walk that found no container construction proves nothing.
	t.Logf("service container law: %d test files parsed, %d container constructions examined", len(files), calls)
	if len(files) < 50 || calls == 0 {
		t.Fatalf("examined too little to judge (%d files, %d calls)", len(files), calls)
	}
}

// #367 (2026-10-14, lane W8-AC): internal/service took 465 s under -race (CI:
// 12+ min, past go test's 10-minute default); 54.6 % of its CPU was
// sqlite Migrate, 40 % of it inside storage.Open's migrate of each unit-test
// container's :memory: database (301 boots). Unit-test containers now open
// through WithDatabaseOpener with a copy of sqlitetest's migrated in-memory
// image (storage-abstraction design, "In-memory test databases").

// The container opens its database through the opener it is given, with its
// own database config, and uses the backend the opener returns.
func TestWithDatabaseOpener_IsTheContainersOpen(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	var got []config.DatabaseConfig
	var returned *storage.Backend
	open := func(_ context.Context, dc config.DatabaseConfig) (*storage.Backend, error) {
		got = append(got, dc)
		returned = storage.NewSQLiteBackend(sqlitetest.Memory(t))
		return returned, nil
	}
	// A caller's options come after the helper's, so this opener wins.
	c, err := newUnitTestContainer(t, cfg, isolatedConfigPath(t), WithDatabaseOpener(open))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Driver != "sqlite" || got[0].Path != ":memory:" {
		t.Fatalf("opener calls %+v, want one with the container's database config", got)
	}
	if c.backend != returned || c.DB != returned.DB {
		t.Fatal("the container does not use the backend its opener returned")
	}
}

// Without an opener the container opens through storage.Open, as the daemon
// does.
func TestNoDatabaseOpener_OpensThroughStorage(t *testing.T) {
	c := &Container{}
	if c.databaseOpener != nil {
		t.Fatal("a container built with no options has an opener")
	}
	if reflect.ValueOf(c.openDatabase()).Pointer() != reflect.ValueOf(storage.Open).Pointer() {
		t.Fatal("with no opener the container does not open through storage.Open")
	}
}

// Every unit-test container takes the opener, and its database is at the
// latest schema and its own: a row written through one container's database
// is not in another's.
func TestNewUnitTestContainer_DatabaseIsAMigratedCopyOfItsOwn(t *testing.T) {
	ctx := context.Background()
	a, err := newUnitTestContainer(t, newComposerWiringTestConfig(t), isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := newUnitTestContainer(t, newComposerWiringTestConfig(t), isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*Container{"a": a, "b": b} {
		if c.databaseOpener == nil {
			t.Fatalf("%s: the unit-test container has no database opener", name)
		}
		if err := c.backend.IsReady(ctx); err != nil {
			t.Fatalf("%s: database not at the latest schema: %v", name, err)
		}
	}
	if _, err := a.DB.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES ('w8ac-container-probe', '2026-10-14T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.DB.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'w8ac-container-probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a row written to one unit-test container's database appeared in another's")
	}
}

// The unit-test opener copies the image only for the in-memory SQLite
// database; a test that boots on a file keeps storage.Open (it migrates and
// creates the file), so file-backed boots are still the production open.
func TestUnitTestDatabaseOpener_FileDatabaseKeepsStorageOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot.db")
	be, err := unitTestDatabaseOpener(t)(sqlitetest.Context(t), config.DatabaseConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("storage.Open did not create the file database: %v", err)
	}
	if err := be.IsReady(context.Background()); err != nil {
		t.Fatalf("file database not migrated: %v", err)
	}
}
