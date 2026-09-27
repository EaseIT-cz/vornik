package forge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/spawn"
)

// gitOutput runs git in gitDir through the process-spawn law's GitWorkspace
// kind (internal/spawn): gitDir must lie under the registered workspace root,
// and a refusal starts nothing.
func gitOutput(ctx context.Context, gitDir string, args ...string) ([]byte, error) {
	cmd, err := spawn.GitWorkspace(ctx, gitDir, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

// WorkspaceHead returns the HEAD commit of the project clone at gitDir: the
// publish source a forge.open_change_request step opens its change request
// from. It lives here, with the handler's other git reads, so the service
// wiring that locates the clone spawns nothing itself (process-spawn law,
// S1b-2).
func WorkspaceHead(ctx context.Context, gitDir string) (string, error) {
	out, err := gitOutput(ctx, gitDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("forge: rev-parse HEAD in %s: %w", gitDir, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("forge: empty HEAD sha in %s", gitDir)
	}
	return sha, nil
}

// commitsBeyondBase returns how many commits sha has beyond the base branch, and
// ok=false when it can't be determined (e.g. the base ref isn't present locally).
// Used to decide whether there's anything to publish: if the child dev-pipeline
// merged no change, HEAD == base and there's nothing to open a change request
// from. Compares against the remote base ref first (origin/<base>, the true PR
// base), falling back to the local base ref.
func commitsBeyondBase(ctx context.Context, gitDir, base, sha string) (int, bool) {
	if strings.TrimSpace(gitDir) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(sha) == "" {
		return 0, false
	}
	for _, ref := range []string{"origin/" + base, base} {
		out, err := gitOutput(ctx, gitDir, "rev-list", "--count", ref+".."+sha)
		if err != nil {
			continue
		}
		if n, convErr := strconv.Atoi(strings.TrimSpace(string(out))); convErr == nil {
			return n, true
		}
	}
	return 0, false
}

// patchFromBase produces a `git format-patch` (mailbox format, `git am`-able) of
// every commit sha has beyond base, so an operator can apply the change by hand
// when the daemon can't push it. Tries the remote base ref first (origin/<base>,
// the true CR base) then the local base, matching commitsBeyondBase. Falls back
// to a plain unified diff if format-patch yields nothing.
func patchFromBase(ctx context.Context, gitDir, base, sha string) ([]byte, error) {
	if strings.TrimSpace(gitDir) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("forge: patch: empty gitDir/base/sha")
	}
	for _, ref := range []string{"origin/" + base, base} {
		if out, err := gitOutput(ctx, gitDir, "format-patch", "--stdout", ref+".."+sha); err == nil && len(out) > 0 {
			return out, nil
		}
	}
	for _, ref := range []string{"origin/" + base, base} {
		if out, err := gitOutput(ctx, gitDir, "diff", ref+".."+sha); err == nil && len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("forge: could not produce a patch for %s..%s in %s", base, sha, gitDir)
}
