// Package starthold reads the start-held marker (GitHub #586): a file that,
// present at process start, holds the daemon from work until an operator
// releases it. The daemon reads it once, before the container is built,
// removes it at the release, and writes the same bytes back only when that
// release's work start fails.
// see LLD § https://docs.vornik.io §13.1
package starthold

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileName is the marker's name in the data directory.
const FileName = "start-held"

// DefaultDeadline applies when the marker names none or names one out of
// range.
const DefaultDeadline = 1800 * time.Second

const (
	minDeadlineSeconds = 60
	maxDeadlineSeconds = 86400
	maxHoldIDLen       = 64
	maxReasonLen       = 200
)

// Hold is what the marker says. Held is false only when the file is absent.
type Hold struct {
	Held     bool
	HoldID   string
	Reason   string
	Deadline time.Duration
	// Since is the deadline's base: created_at, or the process start when
	// created_at is missing, does not parse, or is in the future
	// (BaseFromStart).
	Since         time.Time
	BaseFromStart bool
	// Err says why the marker is malformed; a malformed marker still holds
	// (fail closed).
	Err error
	// Raw is the file's bytes, for the write-back after a failed release.
	Raw []byte
}

// DeadlineAt is when the hold becomes overdue.
func (h Hold) DeadlineAt() time.Time { return h.Since.Add(h.Deadline) }

// Path is the marker's path: in dataDir (VORNIK_DATA_DIR), or beside the
// configuration file when there is none (operator answer 3, 2026-11-24;
// restartgen's rule).
func Path(dataDir, configPath string) string {
	if dataDir != "" {
		return filepath.Join(dataDir, FileName)
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

type markerJSON struct {
	HoldID          string `json:"hold_id"`
	CreatedAt       string `json:"created_at"`
	Reason          string `json:"reason"`
	DeadlineSeconds *int64 `json:"deadline_seconds"`
}

// Read reads the marker at path; now is the process start.
func Read(path string, now time.Time) Hold {
	raw, err := os.ReadFile(path) //nolint:gosec // the daemon's own data dir
	if errors.Is(err, fs.ErrNotExist) {
		return Hold{}
	}
	h := Hold{Held: true, Deadline: DefaultDeadline, Since: now, BaseFromStart: true, Raw: raw}
	if err != nil {
		h.Err = fmt.Errorf("the marker could not be read: %w", err)
		return h
	}
	var m markerJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		h.Err = fmt.Errorf("the marker is not the expected JSON line: %w", err)
		return h
	}
	h.HoldID, h.Reason = m.HoldID, m.Reason
	if created, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil && !created.After(now) {
		h.Since, h.BaseFromStart = created, false
	}
	var problems []error
	if !validHoldID(m.HoldID) {
		problems = append(problems, fmt.Errorf("hold_id must be 1 to %d characters of [A-Za-z0-9._-]", maxHoldIDLen))
	}
	if !validReason(m.Reason) {
		problems = append(problems, fmt.Errorf("reason must be at most %d printable characters", maxReasonLen))
	}
	if d := m.DeadlineSeconds; d != nil {
		if *d < minDeadlineSeconds || *d > maxDeadlineSeconds {
			problems = append(problems, fmt.Errorf("deadline_seconds must be %d to %d; the default %s applies",
				minDeadlineSeconds, maxDeadlineSeconds, DefaultDeadline))
		} else {
			h.Deadline = time.Duration(*d) * time.Second
		}
	}
	h.Err = errors.Join(problems...)
	return h
}

func validHoldID(s string) bool {
	if s == "" || len(s) > maxHoldIDLen {
		return false
	}
	for _, c := range s {
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func validReason(s string) bool {
	if len([]rune(s)) > maxReasonLen {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// Remove deletes the marker. An absent marker is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Restore writes raw back as the marker (0600), through a temporary file
// and a rename, so a crash leaves either no marker or the whole one.
func Restore(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), FileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is durable only once the directory is (review b55d #2).
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
