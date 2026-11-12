package cli

// vornikctl credential import-env (GitHub #626 C; agent-administered Vornik
// design, amendment 2026-11-12): move the secrets a configuration references
// from the daemon's plaintext env files into the encrypted credential store.
// Dry run by default; --apply writes. Names, files and line numbers are
// printed, never a value.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
)

// importEnvPlan is what import-env found (config.PlanEnvImport).
type importEnvPlan struct {
	files []config.EnvImportFile
}

func buildImportEnvPlan(configPath string) (importEnvPlan, error) {
	files, err := config.PlanEnvImport(configPath)
	return importEnvPlan{files: files}, err
}

// printPlan writes one line per entry: what happens, the name, file:line.
func (p importEnvPlan) print(out io.Writer) (moves int) {
	for _, f := range p.files {
		for _, e := range f.Entries {
			if e.Move {
				moves++
				_, _ = fmt.Fprintf(out, "move   %s  (%s:%d)\n", e.Key, f.Path, e.Line)
				continue
			}
			_, _ = fmt.Fprintf(out, "left   %s  (%s:%d)\n       %s\n", e.Key, f.Path, e.Line, e.Reason)
		}
	}
	return moves
}

// credentialPutter stores one operator credential (the admin API's PUT).
type credentialPutter func(name string, value []byte) error

// applyImportEnvPlan stores each name once (its first file's value), then
// rewrites each file once: it reads the file again and drops a line only
// when its name was stored and its value is still the stored value. The new
// content goes to a 0600 temp file renamed over the file only if the file's
// SHA-256 is what it was at that second read. It returns the names stored
// and a report line per file it left unchanged.
func applyImportEnvPlan(plan importEnvPlan, put credentialPutter) (stored []string, report []string) {
	values := map[string]string{}
	failed := map[string]bool{}
	for _, f := range plan.files {
		for _, e := range f.Entries {
			if !e.Move {
				continue
			}
			if _, done := values[e.Key]; done || failed[e.Key] {
				continue
			}
			if err := put(e.Key, []byte(e.Value)); err != nil {
				failed[e.Key] = true
				report = append(report, fmt.Sprintf("%s: not stored (%v); its line stays", e.Key, err))
				continue
			}
			values[e.Key] = e.Value
			stored = append(stored, e.Key)
		}
	}
	for _, f := range plan.files {
		if msg := rewriteEnvFile(f.Path, values); msg != "" {
			report = append(report, msg)
		}
	}
	sort.Strings(stored)
	return stored, report
}

// importEnvBeforeRename runs between the temp file's write and the hash
// check; a test edits the file there to prove the check.
var importEnvBeforeRename = func(string) {}

// rewriteEnvFile drops the lines of path whose key was stored with an equal
// value. It reports a file it had to leave as it was.
func rewriteEnvFile(path string, stored map[string]string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("%s: could not be read again (%v); left as it is", path, err)
	}
	sum := sha256.Sum256(data)
	drop := map[int]bool{}
	var differ []string
	for _, e := range config.ParseEnvEntries(data) {
		v, ok := stored[e.Key]
		switch {
		case ok && v == e.Value:
			drop[e.Line] = true
		case ok:
			differ = append(differ, fmt.Sprintf("%s (line %d)", e.Key, e.Line))
		}
	}
	note := ""
	if len(differ) > 0 {
		note = fmt.Sprintf("%s: kept %s: the value differs from the stored one", path, strings.Join(differ, ", "))
	}
	if len(drop) == 0 {
		return note
	}
	var out bytes.Buffer
	for i, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" || drop[i+1] {
			continue
		}
		out.WriteString(line)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".import-env-*")
	if err != nil {
		return fmt.Sprintf("%s: could not write a temp file (%v); left as it is", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(out.Bytes())
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Sprintf("%s: could not write a temp file (%v); left as it is", path, err)
		}
	} else {
		_ = tmp.Close()
		return fmt.Sprintf("%s: could not set the temp file's mode (%v); left as it is", path, err)
	}
	importEnvBeforeRename(path)
	again, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(again) != sum {
		return fmt.Sprintf("%s: changed during import, re-run (the stored credentials stay; a re-run removes the lines)", path)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Sprintf("%s: could not be replaced (%v); left as it is", path, err)
	}
	return note
}

var credentialImportEnvApply bool
var credentialImportEnvConfig string

var credentialImportEnvCmd = &cobra.Command{
	Use:   "import-env",
	Short: "Move the secrets your configuration uses from env files into the credential store",
	Long: `Reads the daemon's env files (vornik.env, secrets/env, secrets/*.env) and lists
each name: "move" when config.yaml references it outside database:, a project
*_env field names it, or it is in project-secrets.env; "left" otherwise, with
the reason. Names referenced under database: stay on disk: they open the
store. Nothing is written without --apply. Values are never printed.

With --apply, each moved name is stored through the admin API (admin-class
VORNIK_API_KEY, audited). Its line is removed only when the file is unchanged
since it was read and the line's value is the stored one.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path := config.ResolveConfigPath(credentialImportEnvConfig)
		if path == "" {
			return errors.New("no config.yaml found; pass --config")
		}
		plan, err := buildImportEnvPlan(path)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		moves := plan.print(out)
		if !credentialImportEnvApply {
			_, _ = fmt.Fprintf(out, "\n%d name(s) would move. Nothing was written; run again with --apply.\n", moves)
			return nil
		}
		client := ClientFromEnv()
		stored, report := applyImportEnvPlan(plan, func(name string, value []byte) error {
			resp, err := client.DoRaw(http.MethodPut, credentialAPIPath+"/"+name, value, "application/octet-stream")
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return ParseAPIError(resp)
			}
			return nil
		})
		_, _ = fmt.Fprintf(out, "\nstored %d credential(s): %s\n", len(stored), strings.Join(stored, ", "))
		for _, r := range report {
			_, _ = fmt.Fprintln(out, r)
		}
		_, _ = fmt.Fprintln(out, "The running daemon keeps the values it loaded, equal to the stored ones. "+
			"A unit that lists these files as EnvironmentFile= reads them only at start; restart it to drop the old copies from its environment.")
		if len(report) > 0 {
			return errors.New("some names were not moved; see above")
		}
		return nil
	},
}

func init() {
	credentialImportEnvCmd.Flags().BoolVar(&credentialImportEnvApply, "apply", false, "Store the credentials and remove the moved lines (default: print the plan only)")
	credentialImportEnvCmd.Flags().StringVar(&credentialImportEnvConfig, "config", "", "Path to config.yaml (default: the daemon's search order)")
	credentialCmd.AddCommand(credentialImportEnvCmd)
}
