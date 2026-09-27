package configdrift

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/filelock"
)

// The acknowledgement store (drift design, slice C). A tuned host declines
// template changes routinely, so an unsilenceable WARNING would ratchet upward
// until ignored; an ack records the decision against the exact findings the
// operator saw, keyed so a genuinely new finding re-opens it.
//
//	<configs>/.template-acks        the store: one record per acked finding
//	<configs>/.origin/.acks.journal every ack ever written, append-only
//	<configs>/.template-acks.lock   serialises the store's read-modify-write
//
// Record: rel \t class \t key \t regime \t date (RFC 3339, UTC, seconds —
// so dates compare as strings; equal dates tie-break on the whole record).
const (
	ackStoreName   = ".template-acks"
	ackLockName    = ".template-acks.lock"
	ackJournalPath = ".origin/.acks.journal"
)

// ErrNothingToAck means the comparison has no finding for the file, so there
// is no decision to record — an ack is never a blanket pre-approval.
var ErrNothingToAck = errors.New("configdrift: nothing to acknowledge for that file")

// ErrNoBaseline means there is no template baseline at all — nothing to key
// an acknowledgement against. Distinct from a baseline that exists but cannot
// be trusted, because the operator's next step differs in wording, not in
// action: run the installer.
var ErrNoBaseline = errors.New("configdrift: no template baseline has been recorded")

// ErrBaselineNotCurrent means keys computed against an unstamped or stale
// reference
// would describe findings the check does not show, and pruning under it would
// drop valid records.
var ErrBaselineNotCurrent = errors.New("configdrift: the template baseline is not current")

// AckRecord is one acknowledged finding.
type AckRecord struct {
	Rel    string
	Class  AckClass
	Key    string
	Regime string
	Date   string
	// Actor is who acknowledged (drift design slice F): the admin principal,
	// never a credential. "" only on a line written before slice F.
	Actor string
}

func (a AckRecord) id() string {
	return a.Rel + "\t" + string(a.Class) + "\t" + a.Key + "\t" + a.Regime
}

// line is the record's on-disk form. The actor is the sixth field and not
// part of the id; a pre-slice-F record (no actor) keeps its five-field form.
func (a AckRecord) line() string {
	if a.Actor == "" {
		return a.id() + "\t" + a.Date
	}
	return a.id() + "\t" + a.Date + "\t" + a.Actor
}

// SanitizeActor makes an actor safe as a record field: the tab separates
// fields and CR/LF separate records.
func SanitizeActor(actor string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(actor)
}

// Acks is what the check reads: the store and the journal.
type Acks struct {
	store, journal map[string]AckRecord
}

// ReadAcks loads the store and the journal; missing files are empty.
func ReadAcks(configsDir string) (*Acks, error) {
	store, err := readAckFile(filepath.Join(configsDir, ackStoreName))
	if err != nil {
		return nil, err
	}
	journal, err := readAckFile(filepath.Join(configsDir, filepath.FromSlash(ackJournalPath)))
	if err != nil {
		return nil, err
	}
	return &Acks{store: store, journal: journal}, nil
}

// AckStatus is how one finding stands against the acknowledgements, in the
// design's read-side precedence (round 10 F1).
type AckStatus int

const (
	// Unacked means no acknowledgement applies.
	Unacked AckStatus = iota
	// Acked means the key is in the store — suppressed.
	Acked
	// NowClassifiable means a soft finding now exact, acknowledged earlier in the
	// vague regime.
	NowClassifiable
	// AckMissing means in the journal but not the store (a restore dropped it).
	AckMissing
)

// Status reports how a finding with key k in file rel stands, and the date of
// the record that decided it.
func (a *Acks) Status(rel string, k AckKey) (AckStatus, string) {
	st, rec := a.StatusRecord(rel, k)
	return st, rec.Date
}

// StatusRecord is Status with the record that decided it, so the renderer can
// name who acknowledged (drift design slice F): the journal line for
// AckMissing, the newest vague journal line for NowClassifiable.
func (a *Acks) StatusRecord(rel string, k AckKey) (AckStatus, AckRecord) {
	if a == nil {
		return Unacked, AckRecord{}
	}
	rec := AckRecord{Rel: rel, Class: k.Class, Key: k.Key, Regime: k.Regime}
	if r, ok := a.store[rec.id()]; ok {
		return Acked, r
	}
	// THIS finding's own record in the journal outranks the vague pointer
	// below: the journal-ahead-of-store state (a failed store write, a
	// restore) must read as "acknowledgement missing", as designed — not as
	// "now classifiable" pointing at an older vague ack (round 13 N1).
	if r, ok := a.journal[rec.id()]; ok {
		return AckMissing, r
	}
	// Only vague -> exact: the file was acknowledged unclassified and .origin
	// has since appeared. The reverse (exact -> vague, .origin lost) is not
	// "now classifiable" — the old exact ack does not cover the unclassified
	// form, so it falls through (round 11 F2).
	// Sourced from the JOURNAL, not the store: an ack of any other file prunes
	// the stale vague record from the store, which would erase this pointer
	// before the operator read it (round 12 F4). The journal is never pruned,
	// so the sentence lasts until the exact form itself is acknowledged.
	if k.Class == AckSoft && k.Regime == "exact" {
		var newest AckRecord
		for _, r := range a.journal {
			// Different vague records (different keys) can share a date; the
			// journal map has no order, so an equal date breaks on the whole
			// record line, deterministically (review-20260925-e2f6 finding 1).
			if r.Rel == rel && r.Class == AckSoft && r.Regime == "vague" &&
				(r.Date > newest.Date || (r.Date == newest.Date && r.line() > newest.line())) {
				newest = r
			}
		}
		if newest.Date != "" {
			return NowClassifiable, newest
		}
	}
	return Unacked, AckRecord{}
}

// AckResult is what an acknowledgement recorded and which stale records it
// dropped from the store.
type AckResult struct {
	Recorded []AckRecord
	Pruned   []AckRecord
	Date     string
}

// Acknowledge records every current finding of rel (acknowledged or not, so a
// re-ack is idempotent and restores a store a restore emptied), prunes store
// records whose finding no longer exists anywhere, and appends the new records
// to the journal. The store's read-modify-write runs under an exclusive flock,
// so concurrent acks cannot lose one another's records; the store is replaced
// by rename, so an ack is atomic across classes.
//
// current is the caller's currency test (the daemon knows its own revision;
// this package does not). It runs INSIDE the lock, on the same comparison
// that drives the write, so a baseline re-stamped mid-ack cannot slip past a
// check made earlier (round 12 F2). nil means "present is enough".
//
// actor is who acknowledged (drift design slice F); the caller derives it and
// it must not be empty, since "" marks a record written before slice F.
func Acknowledge(configsDir, rel string, now time.Time, current func(*Report) (bool, string), actor string) (*AckResult, error) {
	actor = strings.TrimSpace(SanitizeActor(actor))
	if actor == "" {
		return nil, errors.New("configdrift: an acknowledgement must name its actor")
	}
	var res *AckResult
	err := filelock.Exclusive(filepath.Join(configsDir, ackLockName), func() error {
		r, err := Compare(configsDir)
		if err != nil {
			return err
		}
		switch r.Baseline {
		case BaselineAbsent:
			return ErrNoBaseline
		case BaselineUnstamped:
			return fmt.Errorf("%w: it is unstamped (no revision or no class axis)", ErrBaselineNotCurrent)
		}
		if current != nil {
			if ok, why := current(r); !ok {
				return fmt.Errorf("%w: %s", ErrBaselineNotCurrent, why)
			}
		}
		live, mine := currentKeys(r, rel)
		if len(mine) == 0 {
			return ErrNothingToAck
		}
		storePath := filepath.Join(configsDir, ackStoreName)
		store, err := readAckFile(storePath)
		if err != nil {
			return err
		}
		date := now.UTC().Format(time.RFC3339)
		res = &AckResult{Date: date}
		for id, rec := range store {
			if !live[id] {
				delete(store, id)
				res.Pruned = append(res.Pruned, rec)
			}
		}
		sort.Slice(res.Pruned, func(i, j int) bool { return res.Pruned[i].line() < res.Pruned[j].line() })
		for _, k := range mine {
			rec := AckRecord{Rel: rel, Class: k.Class, Key: k.Key, Regime: k.Regime, Date: date, Actor: actor}
			store[rec.id()] = rec
			res.Recorded = append(res.Recorded, rec)
		}
		sort.Slice(res.Recorded, func(i, j int) bool { return res.Recorded[i].line() < res.Recorded[j].line() })
		// Journal FIRST, then the store, both under the lock (round 11 F1).
		// A failed journal write changes nothing; a store rename that fails
		// after it leaves the journal ahead of the store, which reads as the
		// visible "acknowledgement missing" — never a store record the journal
		// lacks, which would be silent and, after a later restore,
		// unrecoverable.
		if err := appendJournal(configsDir, res.Recorded); err != nil {
			return err
		}
		return writeAckStore(storePath, store)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// currentKeys returns the id of every current finding in the report (what
// the store may keep) and the keys of rel's findings (what this ack records).
func currentKeys(r *Report, rel string) (map[string]bool, []AckKey) {
	live := map[string]bool{}
	var mine []AckKey
	note := func(file string, k AckKey) {
		live[AckRecord{Rel: file, Class: k.Class, Key: k.Key, Regime: k.Regime}.id()] = true
		if file == rel {
			mine = append(mine, k)
		}
	}
	for _, ff := range r.Files {
		for _, k := range ff.Keys() {
			note(ff.Rel, k)
		}
	}
	for _, rm := range r.Removed {
		note(rm.Rel, rm.Key())
	}
	return live, mine
}

// writeAckStore replaces the store durably: the temp file is fsynced before
// the rename and the directory after it, so on power loss the store is the old
// one or the new one — and, because the journal was fsynced first, never a
// record the journal lacks (round 12 F1).
func writeAckStore(path string, store map[string]AckRecord) error {
	lines := make([]string, 0, len(store))
	for _, r := range store {
		lines = append(lines, r.line())
	}
	sort.Strings(lines)
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp) // never leave a temp file behind a failed ack
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func appendJournal(configsDir string, recs []AckRecord) error {
	p := filepath.Join(configsDir, filepath.FromSlash(ackJournalPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(r.line())
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	// Durable before the store is touched: the journal-first order only means
	// something if the journal line survives a power loss the store rename
	// might also survive (round 12 F1).
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The directory too: on the first ack .origin/ and the journal are new
	// entries, and a file fsync does not make its directory entry durable
	// (round 13 N2).
	return syncDir(filepath.Dir(p))
}

func readAckFile(p string) (map[string]AckRecord, error) {
	out := map[string]AckRecord{}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 5 || parts[0] == "" {
			continue
		}
		rec := AckRecord{Rel: parts[0], Class: AckClass(parts[1]), Key: parts[2], Regime: parts[3], Date: parts[4]}
		// Slice F: an optional sixth field, the actor. The parse stays
		// LENIENT (skip only < 5 fields, ignore trailing ones) so a binary
		// from before slice F reads a six-field line as before, and it must
		// never be tightened: the journal is never pruned.
		if len(parts) >= 6 {
			rec.Actor = parts[5]
		}
		// The journal can hold the same id many times; the newest date wins,
		// and on an equal date the LATER line in the file (the journal is
		// append-only, so file order is write order).
		if old, ok := out[rec.id()]; !ok || rec.Date >= old.Date {
			out[rec.id()] = rec
		}
	}
	return out, sc.Err()
}
