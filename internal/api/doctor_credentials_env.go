package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/secretstore"
)

// checkCredentialsInEnvFiles reports the secrets the configuration uses
// that still sit in a plaintext env file (GitHub #626 D; agent-administered
// Vornik design, amendment 2026-11-12). Per name, once:
//   - in a file only: WARNING, move it with vornikctl credential import-env;
//   - in a file and in the store: the env file wins;
//   - the live environment differs from the stored value: shadowed, restart
//     to use the stored value;
//   - in a file, referenced by nothing: unreferenced;
//   - referenced under database: kept on disk by design.
//
// It states how many names it examined. Never a value.
func (h *DoctorHandlers) checkCredentialsInEnvFiles(ctx context.Context) DoctorCheck {
	const name = "credentials_in_env_files"
	path := h.configPath
	if path == "" && h.configDir != "" {
		path = filepath.Join(h.configDir, "config.yaml")
	}
	if path == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "config path not wired"}
	}
	files, err := config.PlanEnvImport(path)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read the configuration: " + err.Error()}
	}
	stored := h.storedCredentialReader()
	seen := map[string]bool{}
	pending := map[string]bool{}
	var items []string
	for _, f := range files {
		for _, e := range f.Entries {
			where := fmt.Sprintf("%s:%d", f.Path, e.Line)
			first := !seen[e.Key]
			seen[e.Key] = true
			if !e.Move {
				if !first {
					continue
				}
				switch e.Reason {
				case config.EnvKeepDatabase:
					items = append(items, fmt.Sprintf("%s (%s): kept on disk by design (it opens the store; see secrets_permissions)", e.Key, where))
				case config.EnvKeepUnreferenced:
					items = append(items, fmt.Sprintf("%s (%s): unreferenced", e.Key, where))
				}
				continue
			}
			value, inStore := stored(ctx, e.Key)
			pending[e.Key] = true
			switch {
			case inStore && e.Value != value && !first:
				// A later file's line: the verb keeps it, so say why (review d2bc F2).
				items = append(items, fmt.Sprintf("%s (%s): the value differs from the stored one; the line stays until you remove it", e.Key, where))
			case !first:
				continue
			case !inStore:
				items = append(items, fmt.Sprintf("%s (%s): in an env file; move it with vornikctl credential import-env", e.Key, where))
			case os.Getenv(e.Key) != "" && os.Getenv(e.Key) != value:
				items = append(items, fmt.Sprintf("%s (%s): shadowed: the daemon's environment holds another value; restart to use the stored value", e.Key, where))
			default:
				items = append(items, fmt.Sprintf("%s (%s): in both: the env file wins; vornikctl credential import-env removes the line", e.Key, where))
			}
		}
	}
	sort.Strings(items)
	warn := len(pending)
	msg := fmt.Sprintf("examined %d name(s) in %d env file(s); %d secret(s) still in a file", len(seen), len(files), warn)
	if warn == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: msg, Items: items}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: msg + "; run vornikctl credential import-env", Items: items}
}

// storedCredentialReader reads one operator credential, or reports it
// absent when the store is not wired or its key does not open.
func (h *DoctorHandlers) storedCredentialReader() func(context.Context, string) (string, bool) {
	none := func(context.Context, string) (string, bool) { return "", false }
	if h.agentSecrets == nil || h.agentSecretKeyPath == "" {
		return none
	}
	key, err := secretstore.LoadKey(h.agentSecretKeyPath)
	if err != nil {
		return none
	}
	st, err := secretstore.New(h.agentSecrets, key)
	if err != nil {
		return none
	}
	return func(ctx context.Context, name string) (string, bool) {
		v, err := st.GetOperatorCredential(ctx, name)
		if err != nil || len(v) == 0 {
			return "", false
		}
		return string(v), true
	}
}
