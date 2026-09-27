// Package configdrift compares a deployed config tree against the baselines
// the installer keeps beside it, for the config_template_drift doctor check
// (https://docs.vornik.io, third
// amendment).
//
// The deployed tree is preserve-existing — correct, because Vornik tunes it
// through approved proposals — so a template FIX never arrives, and nothing
// said so. scripts/config-deploy.sh therefore writes, into the deployed
// configs directory:
//
//	.templates/<rel>     the CURRENT template (overwritten, pruned)
//	.templates/.stamp    the revision that wrote it (absent = unstamped)
//	.templates/.classes  the manifest's tunable axis: name, class, dir|file
//	.templates/.removed  template files the product stopped shipping
//	.origin/<rel>        the template as it stood when the file was created
//	.origin/.index       rel, created-hash, revision, date, created|seeded
//
// This package is pure: it reads those files and the deployed tree and says
// what differs. Status, precedence and wording belong to the doctor check.
package configdrift

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Class is the manifest's tunable axis for a top-level entry.
type Class string

const (
	// Tunable entries (swarms, workflows, project-templates) are tuned in
	// place, so only the template side of the diff is actionable.
	Tunable Class = "tunable"
	// Canonical entries (role-library, pricing.yaml) are not tuning surfaces:
	// any divergence is reported.
	Canonical Class = "canonical"
	// Unknown means the baseline records no trustworthy class axis, so a file
	// cannot be routed. Never rendered as canonical — that would report every tuned
	// file as a canonical divergence (round 8 N4).
	Unknown Class = ""
)

// BaselineState is what the installer's baseline can be trusted for.
type BaselineState int

const (
	// BaselineAbsent means no .templates directory — nothing to compare against.
	BaselineAbsent BaselineState = iota
	// BaselineUnstamped means present, but with no revision stamp or no
	// recorded class axis, so it cannot vouch for itself.
	BaselineUnstamped
	// BaselinePresent means stamped, with its class axis.
	BaselinePresent
)

// MaxHunkLines bounds the lines one finding carries, so a heavily tuned file
// (1,466 differing lines, zero actionable, measured on assistant-swarm.md)
// cannot bury a three-line hard finding.
const MaxHunkLines = 20

// FileFinding is what differs in one deployed file.
type FileFinding struct {
	Rel   string
	Class Class
	// Missing: template lines the deployment lacks (the `d` half) — for a
	// tunable file, a change that never deployed.
	Missing []string
	// CanonicalDiverged: a canonical file differs from its template at all.
	CanonicalDiverged bool
	// Exact reports whether the soft class could be classified against a
	// trustworthy .origin entry.
	Exact bool
	// RemovedByTemplate (exact mode): deployed lines that were in the
	// template when this file was created and are no longer — removals the
	// deployment never took (the max_attempts shape).
	RemovedByTemplate []string
	OriginRevision    string
	// SoftCount / SoftLargest (vague mode): deployed-only lines, counted, and
	// the largest such hunk (bounded) — tuning and template removals cannot be
	// told apart without .origin.
	SoftCount   int
	SoftLargest []string
	// VagueReason says why a file is in the vague form.
	VagueReason string

	// Key material, UNBOUNDED — an ack key over the displayed subset would let
	// a new removal beyond the display bound inherit an old decision.
	missingHunks           [][]string
	missingIdx             [][]int  // template line indices of each hard hunk
	tmplLines              []string // the whole template, retained solely for ExemptHunks' re-diff (a few KB; FileFinding is per-run)
	removedAll, softAll    []string
	deployedHash, tmplHash string
}

// Removed is a template file the product stopped shipping whose deployed copy
// is still present.
type Removed struct {
	Rel      string
	Class    Class
	Revision string
}

// Report is the whole comparison.
type Report struct {
	Baseline BaselineState
	Stamp    string
	// TemplateFiles counts the template side (the coverage contract);
	// DeployedCompared counts those with a deployed counterpart.
	TemplateFiles    int
	DeployedCompared int
	// MissingFiles are templates with no deployed file at all — the maximal
	// missed change.
	MissingFiles []string
	Files        []FileFinding
	Removed      []Removed
}

// metadata names the installer's bookkeeping files inside .templates/.
var metadata = map[string]bool{".stamp": true, ".classes": true, ".removed": true}

type originEntry struct {
	hash, revision string
}

// Compare reads the baselines under configsDir and compares the deployed tree
// against them. A missing baseline is BaselineAbsent, not an error; an error is
// an I/O failure the caller should report as such.
func Compare(configsDir string) (*Report, error) {
	r := &Report{}
	tdir := filepath.Join(configsDir, ".templates")
	if st, err := os.Stat(tdir); err != nil || !st.IsDir() {
		r.Baseline = BaselineAbsent
		return r, nil
	}
	stamp, _ := readTrimmed(filepath.Join(tdir, ".stamp"))
	classes, classesOK := readClasses(filepath.Join(tdir, ".classes"))
	if classesOK && !classesComplete(tdir, classes) {
		// A .classes that does not name every top-level template entry is
		// partial or corrupt: an axis that mis-routes a divergence is worse
		// than none (round 8 N5).
		classes, classesOK = nil, false
	}
	r.Stamp = stamp
	r.Baseline = BaselinePresent
	if stamp == "" || !classesOK {
		r.Baseline = BaselineUnstamped
	}
	origins := readOriginIndex(filepath.Join(configsDir, ".origin", ".index"))

	var rels []string
	err := filepath.WalkDir(tdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(tdir, p)
		rel = filepath.ToSlash(rel)
		// Top-level dot files are the installer's metadata (and any temp file
		// a crashed install left); no manifest entry starts with a dot.
		if metadata[rel] || (!strings.Contains(rel, "/") && strings.HasPrefix(rel, ".")) {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	for _, rel := range rels {
		r.TemplateFiles++
		deployedPath := filepath.Join(configsDir, filepath.FromSlash(rel))
		deployed, err := os.ReadFile(deployedPath)
		if errors.Is(err, fs.ErrNotExist) {
			r.MissingFiles = append(r.MissingFiles, rel)
			continue
		}
		if err != nil {
			return nil, err
		}
		r.DeployedCompared++
		tmpl, err := os.ReadFile(filepath.Join(tdir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		cls := classOf(classes, rel)
		ff := compareFile(rel, cls, tmpl, deployed, configsDir, origins)
		if ff != nil {
			r.Files = append(r.Files, *ff)
		}
	}
	r.Removed = readRemoved(filepath.Join(tdir, ".removed"), configsDir, classes)
	return r, nil
}

func compareFile(rel string, cls Class, tmplBytes, deployedBytes []byte, configsDir string, origins map[string]originEntry) *FileFinding {
	tmpl := splitLines(tmplBytes)
	dep := splitLines(deployedBytes)
	ops := difflib.NewMatcherWithJunk(tmpl, dep, false, nil).GetOpCodes()
	ff := &FileFinding{Rel: rel, Class: cls}
	switch cls {
	case Unknown:
		// Nothing can be said about a file that cannot be routed; the check
		// reports the missing axis instead.
		return nil
	case Canonical:
		for _, op := range ops {
			if op.Tag != 'e' {
				ff.CanonicalDiverged = true
				ff.deployedHash, ff.tmplHash = normHash(deployedBytes), normHash(tmplBytes)
				return ff
			}
		}
		return nil
	}
	hunks, idx, inserted := diffHalves(tmpl, dep, ops)
	ff.missingHunks, ff.missingIdx, ff.tmplLines = hunks, idx, tmpl
	var missing []string
	for _, h := range hunks {
		missing = append(missing, h...)
	}
	ff.Missing = bound(missing)
	if len(inserted) > 0 {
		classifySoft(ff, inserted, tmpl, dep, configsDir, origins)
	}
	if len(ff.Missing) == 0 && len(ff.RemovedByTemplate) == 0 && ff.SoftCount == 0 {
		return nil
	}
	return ff
}

// diffHalves splits the opcodes into the hard `d` half (template lines the
// deployment lacks, documentation filtered) and the soft `a` half (deployed-only
// hunks, blank lines dropped). `replace` is the ignored `c` class.
func diffHalves(tmpl, dep []string, ops []difflib.OpCode) (missing [][]string, missingIdx [][]int, inserted [][]string) {
	fmEnd := frontMatterEnd(tmpl)
	for _, op := range ops {
		switch op.Tag {
		case 'd':
			var hunk []string
			var at []int
			for i := op.I1; i < op.I2; i++ {
				if !isDocumentation(tmpl[i], i, fmEnd) {
					hunk = append(hunk, tmpl[i])
					at = append(at, i)
				}
			}
			if len(hunk) > 0 {
				missing = append(missing, hunk)
				missingIdx = append(missingIdx, at)
			}
		case 'i':
			if hunk := nonBlank(dep[op.J1:op.J2]); len(hunk) > 0 {
				inserted = append(inserted, hunk)
			}
		}
	}
	return missing, missingIdx, inserted
}

// classifySoft fills the soft class: exact against a trustworthy .origin,
// otherwise the vague count and largest hunk, with the reason.
func classifySoft(ff *FileFinding, inserted [][]string, tmpl, dep []string, configsDir string, origins map[string]originEntry) {
	origin, reason := trustedOrigin(ff.Rel, dep, configsDir, origins)
	if origin != nil {
		ff.Exact = true
		ff.OriginRevision = origins[ff.Rel].revision
		// A line the template REMOVED was in .origin and is absent from the
		// current template. A line the template still has, elsewhere, was
		// moved — a tuner's reorder — which is tuning, not a template
		// removal (drift design, round 15 F4).
		inTemplate := map[string]bool{}
		for _, l := range tmpl {
			inTemplate[l] = true
		}
		for _, hunk := range inserted {
			for _, line := range hunk {
				if origin[line] && !inTemplate[line] {
					ff.removedAll = append(ff.removedAll, line)
				}
			}
		}
		ff.RemovedByTemplate = bound(ff.removedAll)
		return
	}
	ff.VagueReason = reason
	largest := 0
	for i, hunk := range inserted {
		ff.softAll = append(ff.softAll, hunk...)
		ff.SoftCount += len(hunk)
		if len(hunk) > len(inserted[largest]) {
			largest = i
		}
	}
	ff.SoftLargest = bound(inserted[largest])
}

func nonBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// trustedOrigin returns the set of lines in .origin/<rel> when the entry can
// be used for classification, or nil and the reason it cannot.
//
// What is tested, per the design's correction: the ENTRY's integrity — at
// create and at seed the deployed file was the template, so the recorded hash
// must equal the hash of .origin/<rel> itself — and a replacement of wholly
// different ancestry (no shared line). A file legitimately tuned after creation
// changes its own hash by design, so lineage in general is not testable, and is
// not claimed.
func trustedOrigin(rel string, deployed []string, configsDir string, origins map[string]originEntry) (map[string]bool, string) {
	entry, ok := origins[rel]
	if !ok {
		return nil, "no .origin entry (created before baselines were kept, or diverged when first seen)"
	}
	b, err := os.ReadFile(filepath.Join(configsDir, ".origin", filepath.FromSlash(rel)))
	if err != nil {
		return nil, ".origin entry present but its file is unreadable"
	}
	if normHash(b) != entry.hash {
		return nil, ".origin failed its integrity check (content does not hash as recorded)"
	}
	set := map[string]bool{}
	for _, l := range splitLines(b) {
		if strings.TrimSpace(l) != "" {
			set[l] = true
		}
	}
	shared := false
	for _, l := range deployed {
		if set[l] {
			shared = true
			break
		}
	}
	if !shared {
		return nil, "the deployed file shares no line with its .origin (replaced by a file of different ancestry)"
	}
	return set, ""
}

// isDocumentation: blank lines, and `#` lines inside YAML front matter, are
// not behaviour. A `#` line in the BODY is prompt content and is kept — the
// boundary is the parse (lines up to the closing ---), not how a line looks.
func isDocumentation(line string, idx, fmEnd int) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true
	}
	return idx < fmEnd && strings.HasPrefix(t, "#")
}

// frontMatterEnd returns the index of the closing `---` of a leading YAML
// front matter block, or 0 when the file has none.
func frontMatterEnd(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return i
		}
	}
	return 0
}

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r", "")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func bound(lines []string) []string {
	if len(lines) > MaxHunkLines {
		return lines[:MaxHunkLines]
	}
	return lines
}

func normHash(b []byte) string {
	h := sha256.Sum256([]byte(strings.ReplaceAll(string(b), "\r", "")))
	return hex.EncodeToString(h[:])
}

func readTrimmed(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readClasses parses .templates/.classes: name \t tunable|canonical \t dir|file.
func readClasses(p string) (map[string]Class, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	out := map[string]Class{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 {
			continue
		}
		switch Class(parts[1]) {
		case Tunable, Canonical:
			out[parts[0]] = Class(parts[1])
		}
	}
	return out, len(out) > 0
}

// classOf resolves a template path's class from its top-level entry: the file
// itself for a top-level file, its first path segment for a directory entry.
// With no axis, or an entry the axis does not name, the class is Unknown.
func classOf(classes map[string]Class, rel string) Class {
	if c, ok := classes[rel]; ok {
		return c
	}
	if i := strings.IndexByte(rel, '/'); i > 0 {
		if c, ok := classes[rel[:i]]; ok {
			return c
		}
	}
	return Unknown
}

// classesComplete reports whether .classes names every top-level entry of the
// template tree.
func classesComplete(tdir string, classes map[string]Class) bool {
	entries, err := os.ReadDir(tdir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if metadata[e.Name()] {
			continue
		}
		if _, ok := classes[e.Name()]; !ok {
			return false
		}
	}
	return true
}

func readOriginIndex(p string) map[string]originEntry {
	out := map[string]originEntry{}
	f, err := os.Open(p)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		out[parts[0]] = originEntry{hash: parts[1], revision: parts[2]}
	}
	return out
}

// readRemoved returns the .removed entries whose deployed copy still exists.
func readRemoved(p, configsDir string, classes map[string]Class) []Removed {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []Removed
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(configsDir, filepath.FromSlash(parts[0]))); err != nil {
			continue
		}
		out = append(out, Removed{Rel: parts[0], Class: classOf(classes, parts[0]), Revision: parts[1]})
	}
	return out
}

// IsBaselineArtifact reports whether rel (relative to the configs directory,
// slash-separated) is one of this machinery's own files — the two baselines
// and the ack store — which every consumer that scans the deployed tree must
// skip. The shell twin is config_is_baseline_artifact in
// scripts/config-deployable.sh.
func IsBaselineArtifact(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range []string{".templates", ".origin"} {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return rel == ".template-acks" || rel == ".template-acks.lock"
}

// AckClass is which of a file's findings an acknowledgement covers.
type AckClass string

const (
	// AckHard is one missed-template-content (`d`) hunk.
	AckHard AckClass = "hard"
	// AckSoft is the file's whole soft class, in one regime.
	AckSoft AckClass = "soft"
	// AckCanonical is a canonical file's divergence as it stands.
	AckCanonical AckClass = "canonical"
	// AckRemoved is a template the product stopped shipping.
	AckRemoved AckClass = "removed"
)

// AckKey identifies one finding for the acknowledgement store. Regime is
// "exact" or "vague" for soft keys and "-" otherwise.
type AckKey struct {
	Class  AckClass
	Key    string
	Regime string
}

// Keys returns the acknowledgement key of every finding in the file (drift
// design, slice C): one per hard hunk — sha256 over its template-side lines as
// a sorted multiset, leading whitespace kept, CR dropped, so a reorder keeps
// the key and a re-indent changes it; one for the whole soft class, tagged
// with its regime, so `.origin` appearing re-keys it; and the deployed and
// template hash pair for a canonical divergence.
func (ff FileFinding) Keys() []AckKey {
	var out []AckKey
	for _, h := range ff.missingHunks {
		out = append(out, AckKey{Class: AckHard, Key: hunkKey(h), Regime: "-"})
	}
	switch {
	case ff.Exact && len(ff.removedAll) > 0:
		out = append(out, AckKey{Class: AckSoft, Key: hunkKey(ff.removedAll), Regime: "exact"})
	case !ff.Exact && len(ff.softAll) > 0:
		out = append(out, AckKey{Class: AckSoft, Key: hunkKey(ff.softAll), Regime: "vague"})
	}
	if ff.CanonicalDiverged {
		out = append(out, AckKey{Class: AckCanonical, Key: ff.deployedHash + "+" + ff.tmplHash, Regime: "-"})
	}
	return out
}

// Key is the acknowledgement key of a removed-template finding: the removing
// revision, so a later re-add and re-removal is a new finding (round 8 N1).
func (rm Removed) Key() AckKey {
	return AckKey{Class: AckRemoved, Key: rm.Revision, Regime: "-"}
}

// hunkKey hashes lines as a sorted multiset: order-insensitive, duplicate- and
// indentation-sensitive, CR-insensitive.
func hunkKey(lines []string) string {
	norm := make([]string, len(lines))
	for i, l := range lines {
		norm[i] = strings.TrimRight(strings.ReplaceAll(l, "\r", ""), " \t")
	}
	sort.Strings(norm)
	h := sha256.Sum256([]byte(strings.Join(norm, "\n")))
	return hex.EncodeToString(h[:])
}

// Hunk is one hard (`d`) hunk for display: its lines, bounded, its key, and
// its index (for ExemptHunks).
type Hunk struct {
	Lines []string
	Key   AckKey
	Index int
}

// HardHunks returns the file's `d` hunks with their acknowledgement keys, so a
// caller can render only the unacknowledged ones.
func (ff FileFinding) HardHunks() []Hunk {
	out := make([]Hunk, 0, len(ff.missingHunks))
	for i, h := range ff.missingHunks {
		out = append(out, Hunk{Lines: bound(h), Key: AckKey{Class: AckHard, Key: hunkKey(h), Regime: "-"}, Index: i})
	}
	return out
}

// ApprovedChange is one applied remove_step / reorder_steps proposal: the
// deployed file just before it applied, and the genome it applied.
type ApprovedChange struct {
	PreApply, PostApply []byte
}

// ExemptHunks returns the indices of the hard hunks an approved change
// explains (drift design, slice E, the positional rule). A hunk is exempt when
// EVERY one of its lines aligns — in a sequence match of the template against
// the pre-apply file — to a pre-apply position the approved change deleted
// (a delete or replace range of pre-apply → post-apply), under ANY of the
// changes. Alignment is positional, not by content: a never-taken template fix
// has no counterpart in the pre-apply file, so it aligns to nothing and stays
// reported even when a line of it is textually identical to one the approved
// change removed elsewhere. Only a FileFinding produced by Compare carries the
// hunk indices and template this needs; any other exempts nothing, so every
// hunk renders.
func (ff FileFinding) ExemptHunks(changes []ApprovedChange) map[int]bool {
	out := map[int]bool{}
	if len(ff.missingIdx) == 0 || len(changes) == 0 {
		return out
	}
	views := make([]exemptView, 0, len(changes))
	for _, c := range changes {
		views = append(views, newExemptView(ff.tmplLines, c))
	}
	for h, idxs := range ff.missingIdx {
		if allExplained(idxs, views) {
			out[h] = true
		}
	}
	return out
}

// exemptView is one approved change seen from the template: where each
// template line aligns in the pre-apply file, and which pre-apply positions
// the change deleted.
type exemptView struct {
	align   map[int]int // template index -> pre-apply index (equal opcodes only)
	deleted map[int]bool
}

func newExemptView(tmpl []string, c ApprovedChange) exemptView {
	pre, post := splitLines(c.PreApply), splitLines(c.PostApply)
	v := exemptView{align: map[int]int{}, deleted: map[int]bool{}}
	for _, op := range difflib.NewMatcherWithJunk(pre, post, false, nil).GetOpCodes() {
		if op.Tag == 'd' || op.Tag == 'r' {
			for j := op.I1; j < op.I2; j++ {
				v.deleted[j] = true
			}
		}
	}
	for _, op := range difflib.NewMatcherWithJunk(tmpl, pre, false, nil).GetOpCodes() {
		if op.Tag == 'e' {
			for k := 0; k < op.I2-op.I1; k++ {
				v.align[op.I1+k] = op.J1 + k
			}
		}
	}
	return v
}

// explains reports whether template line ti aligns into a deleted position.
func (v exemptView) explains(ti int) bool {
	pj, ok := v.align[ti]
	return ok && v.deleted[pj]
}

// allExplained reports whether every template index is explained by some view
// (the union across approved changes).
func allExplained(idxs []int, views []exemptView) bool {
	for _, ti := range idxs {
		explained := false
		for _, v := range views {
			if v.explains(ti) {
				explained = true
				break
			}
		}
		if !explained {
			return false
		}
	}
	return true
}

// SoftKey is the key of the file's soft class, if it has one.
func (ff FileFinding) SoftKey() (AckKey, bool) {
	for _, k := range ff.Keys() {
		if k.Class == AckSoft {
			return k, true
		}
	}
	return AckKey{}, false
}

// CanonicalKey is the key of a canonical divergence, if there is one.
func (ff FileFinding) CanonicalKey() (AckKey, bool) {
	for _, k := range ff.Keys() {
		if k.Class == AckCanonical {
			return k, true
		}
	}
	return AckKey{}, false
}
