package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// The env-import plan (GitHub #626 C, D; agent-administered Vornik design,
// amendment 2026-11-12): which names in the daemon's env files the
// configuration uses as secrets, and so belong in the credential store.
// vornikctl credential import-env acts on it; the doctor's
// credentials_in_env_files check reports it. Pure file reading.

// EnvImportEntry is one line of an env file and whether it moves.
type EnvImportEntry struct {
	Key   string
	Value string // the file's value; never printed
	Line  int
	Move  bool
	// Reason says why a line that does not move stays.
	Reason string
}

// EnvImportFile is one env file's entries, in file order.
type EnvImportFile struct {
	Path    string
	Entries []EnvImportEntry
}

// Reasons a line stays.
const (
	EnvKeepDatabase     = "used by database: (kept on disk by design)"
	EnvKeepInvalidName  = "invalid name"
	EnvKeepUnreferenced = "not referenced"
	EnvKeepOverridden   = "set again later in this file (the later line is the one the daemon uses)"
)

// projectEnvField matches a project YAML's `<name>_env: NAME` field (the
// bare variable a project integration reads its secret from).
var projectEnvField = regexp.MustCompile(`(?m)^\s*[a-z][a-z0-9_]*_env:\s*["']?([A-Z][A-Z0-9_]*)["']?\s*(?:#.*)?$`)

// PlanEnvImport reads configPath, its projects and its env files (EnvFiles
// order) and decides, per line, whether it moves. A name moves when
// config.yaml references it outside its database block, a project *_env
// field names it, or it is in project-secrets.env; never when the database
// block references it, nor when the store would refuse its name.
func PlanEnvImport(configPath string) ([]EnvImportFile, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}
	referenced := map[string]bool{}
	for _, n := range credentialCandidateNames(raw) {
		referenced[n] = true
	}
	database := map[string]bool{}
	for _, n := range DatabaseNames(raw) {
		database[n] = true
	}
	projects, _ := filepath.Glob(filepath.Join(filepath.Dir(configPath), "configs", "projects", "*.yaml"))
	for _, p := range projects {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, m := range projectEnvField.FindAllSubmatch(b, -1) {
			referenced[string(m[1])] = true
		}
	}
	var files []EnvImportFile
	for _, path := range envFileCandidates(configPath) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f := EnvImportFile{Path: path}
		projectSecrets := filepath.Base(path) == "project-secrets.env"
		entries := ParseEnvEntries(data)
		// Within one file the loader keeps a key's last line (review d2bc F1).
		last := map[string]int{}
		for _, e := range entries {
			last[e.Key] = e.Line
		}
		for _, e := range entries {
			ent := EnvImportEntry{Key: e.Key, Value: e.Value, Line: e.Line}
			switch {
			case last[e.Key] != e.Line:
				ent.Reason = EnvKeepOverridden
			case database[e.Key]:
				ent.Reason = EnvKeepDatabase
			case !ValidCredentialName(e.Key):
				ent.Reason = EnvKeepInvalidName
			case !referenced[e.Key] && !projectSecrets:
				ent.Reason = EnvKeepUnreferenced
			default:
				// A name in two files is stored once, with its first
				// file's value; a later line goes only if it is equal.
				ent.Move = true
			}
			f.Entries = append(f.Entries, ent)
		}
		files = append(files, f)
	}
	return files, nil
}
