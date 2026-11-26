package cli

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/archiveutil"
	"vornik.io/vornik/internal/config"

	_ "github.com/lib/pq"
)

var (
	backupOut            string
	restoreIn            string
	restoreForce         bool
	restoreAllowNonEmpty bool
	restoreClean         bool
	backupRestorable     bool
	backupExport         bool
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Create a one-file archive of the vornik deployment",
	Long: `Create a tarball containing:
  - pg_dump of the database
  - the artifacts directory
  - the project workspaces directory (per-project git repos)
  - a snapshot of the main config file + configs/ directory

Output path defaults to ./vornik-backup-YYYYMMDD-HHMMSS.tgz. The archive
is portable across hosts when restored via 'vornikctl restore'.

Requires: pg_dump on PATH with credentials resolved from config.
The pg_dump major version must be at least the PostgreSQL server major version.

Examples:
  vornikctl backup
  vornikctl backup --out /backups/daily.tgz
`,
	RunE: runBackup,
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a vornik deployment from a backup archive",
	Long: `Restore the database, artifacts directory, project workspaces, and
configs from an archive produced by 'vornikctl backup'. The daemon
MUST NOT be running. Requires psql on PATH. Refuses to run unless
--force is set.

Two safety gates run before the restore proceeds:
  1. Schema-presence gate (B-8): if the target already carries a
     vornik-owned schema (migrations rows OR canonical PG types like
     artifact_class), the restore is refused. Override with --clean
     (drops vornik's objects, keeping the schema and its extensions,
     in the restore's own transaction: a failed restore changes
     nothing) or --allow-non-empty (proceeds anyway, usually fails on
     CREATE TYPE collisions).
  2. Row-count gate: refuses targets with non-empty projects/tasks
     tables. Override with --allow-non-empty.

Examples:
  # Fresh target — no vornik schema yet
  vornikctl restore --from vornik-backup-20260501-040000.tgz --force

  # Daemon already migrated the target; wipe + restore
  vornikctl restore --from <archive> --force --clean

  # Explicitly accept collisions (rarely useful)
  vornikctl restore --from <archive> --force --allow-non-empty

See https://docs.vornik.io "Backup and Restore" for the full matrix.
`,
	RunE: runRestore,
}

func init() {
	backupCmd.Flags().StringVar(&backupOut, "out", "", "output archive path (default auto-generated)")
	restoreCmd.Flags().StringVar(&restoreIn, "from", "", "archive path (required)")
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "required — confirms the daemon is stopped and the DB can be overwritten")
	restoreCmd.Flags().BoolVar(&restoreAllowNonEmpty, "allow-non-empty", false, "permit restore into a DB with existing project/task rows")
	backupCmd.Flags().BoolVar(&backupRestorable, "restorable", false, "also include the secrets/ directory beside the config file (the secret store's key, the env files), so the archive restores on another host; the archive is then as sensitive as those secrets")
	backupCmd.Flags().BoolVar(&backupExport, "export", false, "the owner export: no secrets/, the config redacted, and the rows of every secret-material table (keys, tokens, sessions) left out of the dump; restores to an instance whose secrets are set up again")
	restoreCmd.Flags().BoolVar(&restoreClean, "clean", false, "drop vornik's objects (not the schema or its extensions) in the restore's own transaction — a failed restore changes nothing")
	_ = restoreCmd.MarkFlagRequired("from")
	rootCmd.AddCommand(backupCmd)
	rootCmd.AddCommand(restoreCmd)
}

func runBackup(cmd *cobra.Command, args []string) error {
	if backupExport && backupRestorable {
		return errors.New("--export and --restorable refuse each other: an export leaves the secrets out, a restorable backup adds them")
	}
	cfg, configPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	outPath := backupOut
	if outPath == "" {
		outPath = fmt.Sprintf("vornik-backup-%s.tgz", time.Now().Format("20060102-150405"))
	}

	// Stage everything into a temp dir, then tar it up. pg_dump goes in
	// first because it's the most likely to fail (missing tool, bad creds)
	// and we want to fail fast before touching anything else.
	staging, err := os.MkdirTemp("", "vornik-backup-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// 1. pg_dump
	dumpPath := filepath.Join(staging, "db.sql")
	// Use a short-lived 0600 .pgpass file instead of PGPASSWORD in the env:
	// PGPASSWORD leaks into /proc/<pid>/environ where any process running as
	// the same user can read it, and into ps output on some platforms.
	pgpassPath, cleanup, err := writePGPassFile(&cfg.Database)
	if err != nil {
		return fmt.Errorf("write pgpass: %w", err)
	}
	defer cleanup()
	pgEnv := append(os.Environ(), "PGPASSFILE="+pgpassPath)
	pgArgs := []string{
		"-h", cfg.Database.Host,
		"-p", fmt.Sprint(cfg.Database.Port),
		"-U", cfg.Database.User,
		"-d", cfg.Database.Name,
		"--no-owner", "--no-privileges",
		"-f", dumpPath,
	}
	if backupExport {
		pgArgs = append(pgArgs, exportExcludeTableData()...)
	}
	pgCmd := exec.Command("pg_dump", pgArgs...)
	pgCmd.Env = pgEnv
	if out, err := pgCmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "aborting because of server version mismatch") {
			hint := "Install a matching or newer PostgreSQL client and put its pg_dump first on PATH."
			// PostgreSQL reports `server version: 16.15 (...)`; never use
			// the adjacent pg_dump version as the required package version.
			if version := regexp.MustCompile(`server version: ([1-9][0-9]*)\.`).FindSubmatch(out); len(version) > 1 {
				hint += fmt.Sprintf(" On Debian/Ubuntu, install postgresql-client-%s (the PostgreSQL apt repository may be required).", version[1])
			}
			return fmt.Errorf("pg_dump failed: %w\n%s\n%s", err, out, hint)
		}
		return fmt.Errorf("pg_dump failed: %w\n%s", err, out)
	}
	fmt.Printf("✔ database dumped (%d bytes)\n", fileSize(dumpPath))
	if backupExport {
		n, err := scrubDumpFile(dumpPath)
		if err != nil {
			return fmt.Errorf("scrub the dump: %w", err)
		}
		fmt.Printf("✔ dump scrubbed by value: %d line(s) changed\n", n)
	}

	// 2. artifacts dir (optional — may not exist on fresh deployments).
	artifactsSrc := cfg.Storage.ArtifactsPath
	if artifactsSrc != "" {
		if info, err := os.Stat(artifactsSrc); err == nil && info.IsDir() {
			artifactsDst := filepath.Join(staging, "artifacts")
			if err := copyDir(artifactsSrc, artifactsDst); err != nil {
				return fmt.Errorf("copy artifacts: %w", err)
			}
			fmt.Printf("✔ artifacts copied from %s\n", artifactsSrc)
		} else {
			fmt.Printf("  (skipping artifacts: %s not found)\n", artifactsSrc)
		}
	}

	// 3. project workspaces dir (optional — base dir for per-project git repos).
	workspacesSrc := cfg.Runtime.ProjectWorkspacePath
	if workspacesSrc != "" {
		if info, err := os.Stat(workspacesSrc); err == nil && info.IsDir() {
			workspacesDst := filepath.Join(staging, "workspaces")
			if err := copyDir(workspacesSrc, workspacesDst); err != nil {
				return fmt.Errorf("copy project workspaces: %w", err)
			}
			fmt.Printf("✔ project workspaces copied from %s\n", workspacesSrc)
			if backupExport {
				removed, rewritten, err := scrubWorkspaceCredentials(workspacesDst)
				if err != nil {
					return fmt.Errorf("scrub workspace credentials: %w", err)
				}
				fmt.Printf("✔ workspace credentials left out: %d file(s) removed, %d git remote config(s) stripped of userinfo\n", removed, rewritten)
			}
		} else {
			fmt.Printf("  (skipping project workspaces: %s not found)\n", workspacesSrc)
		}
	}

	// 4. config snapshot: main file + configs/ sibling tree; for an export,
	// redacted (#581 D5).
	if configPath != "" && backupExport {
		if err := stageExportConfig(configPath, staging); err != nil {
			return err
		}
	} else if configPath != "" {
		if err := copyFile(configPath, filepath.Join(staging, "config.yaml")); err != nil {
			return fmt.Errorf("copy config: %w", err)
		}
		fmt.Printf("✔ config file copied\n")

		if dir := resolveConfigsDir(configPath); dir != "" {
			if err := copyDir(dir, filepath.Join(staging, "configs")); err != nil {
				return fmt.Errorf("copy configs dir: %w", err)
			}
			fmt.Printf("✔ configs dir copied from %s\n", dir)
		}
	}

	// 4b. --restorable (GitHub #581 D4): secrets/, at 0600, so the sealed
	// secrets open on a replacement host.
	if backupRestorable {
		if configPath == "" {
			return errors.New("--restorable: no config file path, so no secrets/ directory beside it")
		}
		n, err := stageSecrets(secretsDirFor(configPath), staging)
		if err != nil {
			return err
		}
		fmt.Printf("✔ %d secret file(s) added from %s: this archive is as sensitive as those secrets\n", n, secretsDirFor(configPath))
	}

	// 5. Marker file so restore can validate it's a real vornik archive.
	if err := os.WriteFile(filepath.Join(staging, "VORNIK_BACKUP"), []byte("vornik-backup-v1\n"), 0o644); err != nil {
		return err
	}

	// 6. tar.gz the staging dir, into a file that is 0600 before a byte is
	// written: it holds the database (#581 D4).
	if err := createPrivateFile(outPath); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := tarGzDir(staging, outPath); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	fmt.Printf("\nbackup written: %s (%d bytes)\n", outPath, fileSize(outPath))
	return nil
}

func runRestore(cmd *cobra.Command, args []string) error {
	if !restoreForce {
		return fmt.Errorf("--force is required. stop the vornik daemon and confirm you want to overwrite the target database")
	}
	cfg, configPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// B-8: schema-presence guard. The fresh-install path
	// (`make infra-create` → `vornik starts` → migrations run →
	// operator runs `vornikctl restore`) leaves the target with a
	// full migration-applied schema but ZERO business rows. The
	// row-count gate below (projects / tasks) passes, then psql
	// hits `CREATE TYPE artifact_class already exists` ~line 74 of
	// the dump and dies, leaving a half-restored target. The
	// schema-presence probe closes that gap.
	//
	// Three behaviours:
	//   - --clean: drop the public schema first, then proceed. The
	//     documented "wipe + restore" path for the fresh-install
	//     failure mode.
	//   - --allow-non-empty: explicit operator override. Restore
	//     proceeds even with schema loaded; psql still likely
	//     errors but the operator chose to try.
	//   - neither: refuse with a clear error naming both recovery
	//     paths.
	//
	// --clean drops nothing here. It runs as cleanTargetSQL inside the
	// replay's own transaction below, so a replay that fails leaves the
	// target as it was (GitHub #864: a separate, committed drop emptied the
	// database whenever the replay then failed).
	if !restoreClean && !restoreAllowNonEmpty {
		if err := checkTargetSchemaAbsent(&cfg.Database); err != nil {
			return fmt.Errorf("%w\n\nrecovery paths:\n"+
				"  - re-run with --clean to drop + recreate the schema, OR\n"+
				"  - manually `DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION %s;`, OR\n"+
				"  - re-run with --allow-non-empty if the schema came from the same vornik version\n"+
				"    (will likely still fail on CREATE TYPE collisions — see https://docs.vornik.io)",
				err, cfg.Database.User)
		}
	}

	// Non-empty-target guard: refuse to overwrite a populated DB
	// unless --allow-non-empty is set. The old behaviour was that
	// psql -f would happily clobber existing tables; an operator
	// running `vornikctl restore --force` against the wrong DB had
	// no safety net. Now we count rows in projects + tasks; either
	// > 0 trips the guard. (Migrations / schema_version are
	// expected; we don't probe them.)
	//
	// Skipped with --clean: the clean empties these tables in the replay's
	// own transaction.
	if !restoreAllowNonEmpty && !restoreClean {
		if err := checkTargetEmpty(&cfg.Database); err != nil {
			return fmt.Errorf("%w (pass --allow-non-empty to override)", err)
		}
	}

	staging, err := os.MkdirTemp("", "vornik-restore-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := untarGz(restoreIn, staging); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "VORNIK_BACKUP")); err != nil {
		return fmt.Errorf("not a vornik backup archive (missing VORNIK_BACKUP marker)")
	}
	fmt.Println("✔ archive validated")

	// Restore the database with psql in a single transaction so a
	// mid-restore failure leaves the target consistent (--set
	// ON_ERROR_STOP=on aborts on the first error; -1 wraps the
	// whole file in BEGIN/COMMIT). Atomicity goal from
	// saas-readiness §3.13.
	//
	// With --clean the transaction runs cleanTargetSQL first, so the clean
	// and the replay commit together or not at all (GitHub #864).
	replayPath := filepath.Join(staging, "db.restore.sql")
	if err := writeRestoreDump(filepath.Join(staging, "db.sql"), replayPath); err != nil {
		return err
	}
	pgpassPath, cleanup, err := writePGPassFile(&cfg.Database)
	if err != nil {
		return fmt.Errorf("write pgpass: %w", err)
	}
	defer cleanup()
	pgEnv := append(os.Environ(), "PGPASSFILE="+pgpassPath)
	pgArgs := []string{
		"-h", cfg.Database.Host,
		"-p", fmt.Sprint(cfg.Database.Port),
		"-U", cfg.Database.User,
		"-d", cfg.Database.Name,
		"--single-transaction",
		"--set", "ON_ERROR_STOP=on",
	}
	if restoreClean {
		cleanPath := filepath.Join(staging, "clean.sql")
		if err := os.WriteFile(cleanPath, []byte(cleanTargetSQL), 0o600); err != nil {
			return fmt.Errorf("write the clean step: %w", err)
		}
		pgArgs = append(pgArgs, "-f", cleanPath)
	}
	pgArgs = append(pgArgs, "-f", replayPath)
	pgCmd := exec.Command("psql", pgArgs...)
	pgCmd.Env = pgEnv
	if out, err := pgCmd.CombinedOutput(); err != nil {
		hint := ""
		if strings.Contains(string(out), "permission denied to create extension") {
			hint = "\nthe database has lost an extension the dump needs, and only a superuser can create it: " +
				"run CREATE EXTENSION <name> in this database as a superuser (or re-run db-bootstrap), then restore again"
		}
		return fmt.Errorf("psql restore failed, the database is unchanged: %w\n%s%s", err, out, hint)
	}
	if restoreClean {
		fmt.Println("✔ target cleaned and database restored (one transaction)")
	} else {
		fmt.Println("✔ database restored (single-transaction)")
	}

	// Restore artifacts.
	artifactsSrc := filepath.Join(staging, "artifacts")
	if info, err := os.Stat(artifactsSrc); err == nil && info.IsDir() {
		if cfg.Storage.ArtifactsPath == "" {
			fmt.Println("  (skipping artifacts: no storage.artifacts_path configured)")
		} else {
			if err := restoreTree(artifactsSrc, cfg.Storage.ArtifactsPath); err != nil {
				return fmt.Errorf("restore artifacts: %w", err)
			}
			fmt.Printf("✔ artifacts restored to %s\n", cfg.Storage.ArtifactsPath)
		}
	}

	// Restore project workspaces.
	workspacesSrc := filepath.Join(staging, "workspaces")
	if info, err := os.Stat(workspacesSrc); err == nil && info.IsDir() {
		if cfg.Runtime.ProjectWorkspacePath == "" {
			fmt.Println("  (skipping project workspaces: no runtime.project_workspace_path configured)")
		} else {
			if err := restoreTree(workspacesSrc, cfg.Runtime.ProjectWorkspacePath); err != nil {
				return fmt.Errorf("restore project workspaces: %w", err)
			}
			fmt.Printf("✔ project workspaces restored to %s\n", cfg.Runtime.ProjectWorkspacePath)
		}
	}

	// secrets/ from a --restorable archive (#581 D4), into the directory
	// provision made, with its owner.
	if info, err := os.Stat(filepath.Join(staging, "secrets")); err == nil && info.IsDir() {
		if configPath == "" {
			return errors.New("restore secrets: no config file path, so no secrets/ directory to restore into")
		}
		n, err := restoreSecrets(filepath.Join(staging, "secrets"), secretsDirFor(configPath))
		if err != nil {
			return fmt.Errorf("restore secrets: %w", err)
		}
		fmt.Printf("✔ %d secret file(s) restored to %s\n", n, secretsDirFor(configPath))
	}

	fmt.Println("\nrestore complete. Config files in the archive were NOT applied — compare against your current deployment manually and merge as needed.")
	return nil
}

// The tar.gz packaging + safe-extraction helpers moved to
// internal/archiveutil so the support-report bundle builder
// (internal/api + internal/cli/support_report.go) can reuse the SAME
// path-traversal / symlink guards rather than duplicating them (see
// https://docs.vornik.io §4.1/§7). These thin
// wrappers keep the existing call sites + tests in this package
// unchanged.

func copyFile(src, dst string) error { return archiveutil.CopyFile(src, dst) }

func copyDir(src, dst string) error { return archiveutil.CopyDir(src, dst) }

// restoreEUID and restoreLchown are restore_owner_706_test.go's seams.
var (
	restoreEUID   = os.Geteuid
	restoreLchown = os.Lchown
)

// restoreTree copies src into dst and, as root, gives the destination and
// every copied directory and regular file the owner of dst's parent
// (Lchown; archiveutil.CopyDir skips links). On a package install the parent
// is /var/lib/vornik, the service user's, so the daemon can write what was
// restored; before GitHub #706 only the start-time chown walk handed it back
// (enterprise-packaging-design.md amendment 2026-11-14). Run as another
// user, it changes no owner.
func restoreTree(src, dst string) error {
	if err := copyDir(src, dst); err != nil {
		return err
	}
	if restoreEUID() != 0 {
		return nil
	}
	info, err := os.Stat(filepath.Dir(filepath.Clean(dst)))
	if err != nil {
		return fmt.Errorf("owner of %s: %w", filepath.Dir(dst), err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("owner of %s: no uid on this platform; the restored tree kept the copy's owner", filepath.Dir(dst))
	}
	uid, gid := int(st.Uid), int(st.Gid)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if err := restoreLchown(filepath.Join(dst, rel), uid, gid); err != nil {
			return fmt.Errorf("give %s to %d:%d: %w", filepath.Join(dst, rel), uid, gid, err)
		}
		return nil
	})
}

func tarGzDir(dir, out string) error { return archiveutil.TarGzDir(dir, out) }

func untarGz(archive, dir string) error { return archiveutil.UntarGz(archive, dir) }

func fileSize(p string) int64 { return archiveutil.FileSize(p) }

// checkTargetEmpty connects to the target DB and refuses the
// restore when projects or tasks already hold rows. Returns nil
// when the DB looks fresh — either both probe tables are empty
// or the tables don't exist yet (brand-new install before
// migrations).
//
// Why these two tables: projects is the operator's primary
// identity surface (anything in it is operator-curated content),
// and tasks is the only other table that could realistically
// hold legitimate work in a "fresh" install. Counting both keeps
// the guard tight without enumerating every table.
//
// Implementation: two queries per table. First a to_regclass
// probe (returns NULL when the table doesn't exist); then a
// COUNT only when the table exists. Keeps the SQL readable and
// avoids the awkward "COALESCE around a subquery in a WHERE
// clause" form.
func checkTargetEmpty(db *config.DatabaseConfig) error {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		db.Host, db.Port, db.User, db.Password, db.Name, fallbackSSLMode(db.SSLMode))
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open target database for non-empty probe: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Allowlist for defensive table-name interpolation. The two
	// names are compile-time constants in this function; the
	// allowlist exists so a future caller can't widen the call
	// site without thinking about SQL-injection safety.
	allow := map[string]struct{}{"projects": {}, "tasks": {}}
	for _, table := range []string{"projects", "tasks"} {
		if _, ok := allow[table]; !ok {
			return fmt.Errorf("internal: refusing to probe unknown table %q", table)
		}
		var exists bool
		if err := conn.QueryRow(
			"SELECT to_regclass('public.' || $1) IS NOT NULL",
			table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("probe %s existence: %w", table, err)
		}
		if !exists {
			continue
		}
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return fmt.Errorf("probe %s row count: %w", table, err)
		}
		if n > 0 {
			return fmt.Errorf("refusing restore: target database has %d %s row(s)", n, table)
		}
	}
	return nil
}

// checkTargetSchemaAbsent (B-8) returns an error when the target
// database already carries a vornik-owned schema — either because
// the daemon started and ran its migrations, or because a previous
// restore left tables behind. The detection is two-pronged so it
// catches both the migrations-path (a populated `migrations` table)
// and the bootstrap-SQL-path (typed enum like artifact_class
// already exists even when `migrations` is empty).
//
// Returns nil when the target looks unconditionally safe to
// restore into: no `migrations` rows AND none of the canonical
// vornik-owned types present.
func checkTargetSchemaAbsent(db *config.DatabaseConfig) error {
	connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		db.Host, db.Port, db.User, db.Password, db.Name, fallbackSSLMode(db.SSLMode))
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("open target database for schema probe: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Probe 1: the migrations bookkeeping table. If it exists with
	// rows, the daemon has applied at least one migration against
	// this database — the schema is loaded.
	var migrationsExists bool
	if err := conn.QueryRow(
		"SELECT to_regclass('public.migrations') IS NOT NULL",
	).Scan(&migrationsExists); err != nil {
		return fmt.Errorf("probe migrations existence: %w", err)
	}
	if migrationsExists {
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM migrations").Scan(&n); err != nil {
			return fmt.Errorf("probe migrations row count: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("refusing restore: target database has %d applied migration(s) — schema already loaded", n)
		}
	}

	// Probe 2: a small allowlist of vornik-owned PG types. These
	// come from the bootstrap SQL (deployments/postgres/schema/
	// 001_initial.sql) and a `CREATE TYPE` in the dump can't be
	// idempotent — psql hits "already exists" and bails out.
	// Allowlist of canonical types kept narrow so future operator
	// types in the same DB don't trip the gate.
	vornikTypes := []string{"artifact_class", "task_status", "execution_status"}
	for _, typ := range vornikTypes {
		var exists bool
		if err := conn.QueryRow(
			"SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = $1)",
			typ,
		).Scan(&exists); err != nil {
			return fmt.Errorf("probe pg_type %s: %w", typ, err)
		}
		if exists {
			return fmt.Errorf("refusing restore: target database already defines vornik type %q", typ)
		}
	}
	return nil
}

// cleanTargetSQL is --clean (B-8, rewritten for GitHub #864). It runs as the
// first file of the replay's single transaction and drops, with CASCADE, every
// object in public that is not a member of an extension: tables (plain,
// partitioned, foreign), views, materialized views, sequences, routines,
// standalone types and domains, and text-search configurations. The schema
// and the extensions stay.
//
// It used to DROP SCHEMA public CASCADE in a transaction of its own. That also
// dropped the vector extension, which the superuser creates (db-bootstrap) and
// the daemon's role cannot recreate, and it committed before the replay ran: a
// replay that then failed left an empty database. An object here that the
// role cannot drop now fails the one transaction, and nothing changes.
const cleanTargetSQL = `DO $vornik_clean$
DECLARE
  stmts text[];
  stmt  text;
BEGIN
  -- Every statement is built BEFORE anything is dropped: a name read from
  -- the catalogue after an earlier CASCADE removed the object prints as its
  -- OID, which is not a name (GitHub #864 follow-up). IF EXISTS then skips
  -- what an earlier CASCADE already removed. Order: relations, sequences,
  -- routines, types, text-search configurations.
  SELECT coalesce(array_agg(q ORDER BY ord, n), '{}') INTO stmts FROM (
    SELECT 1 AS ord, c.oid::regclass::text AS n, format('DROP %s IF EXISTS %s CASCADE', CASE c.relkind
        WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END,
        c.oid::regclass::text) AS q
      FROM pg_class c
     WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r','p','f','v','m')
       AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 2, c.oid::regclass::text, format('DROP SEQUENCE IF EXISTS %s CASCADE', c.oid::regclass::text)
      FROM pg_class c
     WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'S'
       AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 3, p.oid::regprocedure::text, format('DROP %s IF EXISTS %s CASCADE', CASE p.prokind
        WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END, p.oid::regprocedure::text)
      FROM pg_proc p
     WHERE p.pronamespace = 'public'::regnamespace
       AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 4, t.oid::regtype::text, format('DROP %s IF EXISTS %s CASCADE', CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END,
        t.oid::regtype::text)
      FROM pg_type t
     WHERE t.typnamespace = 'public'::regnamespace AND t.typtype IN ('e','c','d','r','m')
       AND (t.typtype <> 'c' OR (SELECT relkind FROM pg_class WHERE oid = t.typrelid) = 'c')
       AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 5, format('%I.%I', n.nspname, c.cfgname), format('DROP TEXT SEARCH CONFIGURATION IF EXISTS %I.%I CASCADE', n.nspname, c.cfgname)
      FROM pg_ts_config c JOIN pg_namespace n ON n.oid = c.cfgnamespace
     WHERE c.cfgnamespace = 'public'::regnamespace
       AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_ts_config'::regclass AND d.objid = c.oid AND d.deptype = 'e')
  ) AS s;
  FOREACH stmt IN ARRAY stmts LOOP
    EXECUTE stmt;
  END LOOP;
END
$vornik_clean$;
`

// writeRestoreDump copies a pg_dump plain-SQL file to out without the two
// statements a restore must not replay (GitHub #864):
//
//   - COMMENT ON EXTENSION: only the extension's owner may run it, and on a
//     packaged instance the superuser owns vector, so the daemon's role fails
//     ("must be owner of extension vector"). The comment is the extension's
//     own, written again by CREATE EXTENSION; nothing of vornik's is lost.
//   - SET transaction_timeout = 0: pg_dump 17 and later write it, and a 16
//     server refuses the parameter. 0 is the default, so dropping it changes
//     nothing on a server that knows it.
//
// The filter runs at restore time, so archives written before it restore.
// A COMMENT ON EXTENSION is dropped as a whole statement: pg_dump writes the
// comment's text verbatim, so it can span lines, and the statement ends at
// the first line that ends with ";" outside a string literal (an even count
// of single quotes so far; two apostrophes are a quote inside one). A dump
// that ends inside one refuses the restore before anything runs.
func writeRestoreDump(in, out string) error {
	src, err := os.Open(in)
	if err != nil {
		return fmt.Errorf("open the dump: %w", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create the replay copy: %w", err)
	}
	r, w := bufio.NewReaderSize(src, 1<<20), bufio.NewWriterSize(dst, 1<<20)
	inComment, quotes, startLine := false, 0, 0
	for lineNo := 1; ; lineNo++ {
		line, readErr := r.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			_ = dst.Close()
			return fmt.Errorf("read the dump: %w", readErr)
		}
		body := strings.TrimRight(line, "\r\n")
		switch {
		case inComment || strings.HasPrefix(body, "COMMENT ON EXTENSION "):
			if !inComment {
				inComment, quotes, startLine = true, 0, lineNo
			}
			quotes += strings.Count(body, "'")
			if quotes%2 == 0 && strings.HasSuffix(body, ";") {
				inComment = false
			}
		case body == "SET transaction_timeout = 0;":
		default:
			if _, err := w.WriteString(line); err != nil {
				_ = dst.Close()
				return fmt.Errorf("write the replay copy: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if inComment {
		_ = dst.Close()
		return fmt.Errorf("the dump ends inside the COMMENT ON EXTENSION that starts at line %d; refusing to replay it (nothing was changed)", startLine)
	}
	if err := w.Flush(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("write the replay copy: %w", err)
	}
	return dst.Close()
}

// fallbackSSLMode picks a safe default when the config didn't set
// one. Mirrors the daemon's connection-builder default.
func fallbackSSLMode(in string) string {
	if strings.TrimSpace(in) == "" {
		return "disable"
	}
	return in
}

// writePGPassFile writes a short-lived .pgpass-format file holding the
// database password with 0600 permissions, and returns the path + a
// cleanup callback the caller must defer. pg_dump / psql pick the file up
// via PGPASSFILE, so the password never travels through the process
// environment.
func writePGPassFile(db *config.DatabaseConfig) (string, func(), error) {
	f, err := os.CreateTemp("", "vornik-pgpass-*")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	// .pgpass format: host:port:database:user:password. Colons and
	// backslashes in any field must be backslash-escaped.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `:`, `\:`)
		return s
	}
	line := fmt.Sprintf("%s:%d:%s:%s:%s\n",
		esc(db.Host), db.Port, esc(db.Name), esc(db.User), esc(db.Password))
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}
