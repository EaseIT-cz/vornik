package autonomy

import (
	"os"

	"vornik.io/vornik/internal/spawn"
)

// The process-spawn law's GitWorkspace and GitHTTPBackend kinds refuse a
// directory outside a registered workspace root (design "S1b-2, as built");
// the daemon registers runtime.project_workspace_path. These tests run git in
// t.TempDir() repositories, so the temp root is theirs.
func init() {
	if err := spawn.RegisterWorkspaceRoot(os.TempDir()); err != nil {
		panic(err)
	}
}
