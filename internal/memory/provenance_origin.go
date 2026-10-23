package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// Read-only views of origin/main for the operator backfill (design 22,
// "Amendment 2026-10-21 (#482)", part 5): its base-name lookup and its
// content sample. The daemon never calls them.

// OriginMainEligibleFiles lists every eligible file at origin/main in the
// project's clone, path to blob id: the enumerated single files that exist
// as regular files, and the regular .md files directly under the design
// directory.
func (v *ProvenanceVerifier) OriginMainEligibleFiles(ctx context.Context, projectID string) (map[string]string, error) {
	dir, head, err := v.originMain(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range ProvenanceEligibleFiles() {
		blob, reason, err := v.lookupBlob(ctx, dir, head, f)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			out[f] = blob
		}
	}
	treeID, reason, err := v.lookupTree(ctx, dir, head, ProvenanceLLDDir)
	if err != nil || reason != "" {
		return out, err
	}
	data, err := v.Git(ctx, dir, "cat-file", "tree", treeID)
	if err != nil {
		return nil, err
	}
	entries, err := parseTree(data, len(head)/2)
	if err != nil {
		return nil, err
	}
	for name, e := range entries {
		p := ProvenanceLLDDir + "/" + name
		if (e.mode == "100644" || e.mode == "100755") && IsProvenanceEligiblePath(p) {
			out[p] = e.id
		}
	}
	return out, nil
}

// ReadOriginBlob returns a blob's bytes from the project's clone.
func (v *ProvenanceVerifier) ReadOriginBlob(ctx context.Context, projectID, blobID string) ([]byte, error) {
	if !isHexID(blobID) {
		return nil, fmt.Errorf("not an object id: %q", blobID)
	}
	dir, _, err := v.originMain(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return v.Git(ctx, dir, "cat-file", "blob", blobID)
}

// originMain resolves a safe clone and its origin/main commit, with the same
// clone checks as evaluate.
func (v *ProvenanceVerifier) originMain(ctx context.Context, projectID string) (string, string, error) {
	if v == nil || v.CloneDir == nil || v.Git == nil {
		return "", "", errors.New(ProvReasonNoClone)
	}
	dir := v.CloneDir(projectID)
	if dir == "" {
		return "", "", errors.New(ProvReasonNoClone)
	}
	if reason := cloneUnsafe(dir); reason != "" {
		return "", "", errors.New(reason)
	}
	if out, err := v.Git(ctx, dir, "rev-parse", "--glob=refs/replace/*"); err != nil || strings.TrimSpace(string(out)) != "" {
		return "", "", errors.New(ProvReasonUnsafeClone)
	}
	branch, ok := v.trustedBranch(ctx, dir, projectID, "")
	if !ok {
		return "", "", errors.New(ProvReasonNoOriginMain)
	}
	out, err := v.Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	head := strings.TrimSpace(string(out))
	if err != nil || !isHexID(head) {
		return "", "", errors.New(ProvReasonNoOriginMain)
	}
	return dir, head, nil
}

// lookupTree is lookupBlob for a directory: every component must be a tree.
func (v *ProvenanceVerifier) lookupTree(ctx context.Context, dir, commit, p string) (string, string, error) {
	raw, err := v.Git(ctx, dir, "cat-file", "commit", commit)
	if err != nil {
		return "", "", err
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	id, ok := strings.CutPrefix(string(line), "tree ")
	if !ok || !isHexID(id) {
		return "", "", errors.New("commit object has no tree line")
	}
	for _, seg := range strings.Split(p, "/") {
		data, err := v.Git(ctx, dir, "cat-file", "tree", id)
		if err != nil {
			return "", "", err
		}
		entries, err := parseTree(data, len(commit)/2)
		if err != nil {
			return "", "", err
		}
		e, found := entries[seg]
		if !found {
			return "", ProvReasonNotFound, nil
		}
		if e.mode != "40000" {
			return "", ProvReasonNotRegularFile, nil
		}
		id = e.id
	}
	return id, "", nil
}
