// Package benchlearning holds the learning study's freeze (GitHub #809;
// agent-benchmark design, Amendment 2026-11-18 (#809), change 3 and its
// revision). Learning stops between the warm-up and the measured pass.
// Ticks run through a Gate; Freeze takes the lock exclusively and returns
// after any tick in flight ends.
// The freeze is a file in the bench daemon's data directory, so it survives
// a restart. Since #890 (design §12.37) it names the database it was set on:
// a study recreates the database for each arm, and a freeze from another
// database is set aside rather than freezing a fresh arm. A nil Gate (every
// daemon that is not a bench) always runs.
package benchlearning

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FrozenFile is the freeze marker's name in the data directory.
const FrozenFile = "bench-learning-frozen"

// Gate lets learning ticks run until the freeze.
type Gate struct {
	mu       sync.RWMutex
	path     string
	database string
	frozenAt time.Time
}

// Open reads the freeze marker from dataDir for the database named by
// database ("database <name> oid <oid>", which changes when the database is
// recreated). note is "" or one line for the caller to log: a marker from
// another database was set aside (renamed <marker>.stale-<oid>, kept for
// audit, learning unfrozen), or a marker from before #890 names no database
// and stays frozen.
func Open(dataDir, database string) (g *Gate, note string, err error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, "", errors.New("the bench freeze needs its database's identity")
	}
	g = &Gate{path: filepath.Join(dataDir, FrozenFile), database: database}
	raw, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return g, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the bench freeze marker: %w", err)
	}
	timeLine, owner, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(timeLine))
	if err != nil {
		return nil, "", fmt.Errorf("the bench freeze marker %s holds no time: %w", g.path, err)
	}
	owner = strings.TrimSpace(owner)
	switch {
	case owner == "":
		g.frozenAt = at
		return g, fmt.Sprintf("learning stays frozen: %s was written before #890 and names no database. Starting a new arm on a recreated database? Delete it and restart", g.path), nil
	case owner != database:
		stale := g.path + ".stale-" + oidOf(owner)
		if _, err := os.Stat(stale); err == nil {
			stale += fmt.Sprintf(".%d", time.Now().UnixNano()) // keep every audit copy
		}
		if err := os.Rename(g.path, stale); err != nil {
			return nil, "", fmt.Errorf("set aside the bench freeze marker of %q: %w", owner, err)
		}
		return g, fmt.Sprintf("bench freeze of %s (frozen %s) set aside as %s: this is %s, a fresh arm, so learning is not frozen", owner, at.Format(time.RFC3339), stale, database), nil
	}
	g.frozenAt = at
	return g, "", nil
}

// oidOf is the oid in an identity line, or "unknown".
func oidOf(identity string) string {
	f := strings.Fields(identity)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "oid" {
			return f[i+1]
		}
	}
	return "unknown"
}

// Do runs fn under the shared lock unless learning is frozen, and reports
// whether it ran.
func (g *Gate) Do(fn func()) bool {
	if g == nil {
		fn()
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.frozenAt.IsZero() {
		return false
	}
	fn()
	return true
}

// Freeze stops learning: it waits for any tick in flight, writes the marker,
// and returns the freeze time. A second call returns the first time.
func (g *Gate) Freeze() (time.Time, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.frozenAt.IsZero() {
		return g.frozenAt, nil
	}
	at := time.Now().UTC()
	if err := writeAtomic(g.path, []byte(at.Format(time.RFC3339Nano)+"\n"+g.database+"\n")); err != nil {
		return time.Time{}, fmt.Errorf("write the bench freeze marker: %w", err)
	}
	g.frozenAt = at
	return at, nil
}

// FrozenAt reports the freeze time, if learning is frozen.
func (g *Gate) FrozenAt() (time.Time, bool) {
	if g == nil {
		return time.Time{}, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.frozenAt, !g.frozenAt.IsZero()
}

// writeAtomic writes data to a temporary name beside path, syncs it and
// renames it into place, so a crash leaves either no marker or a whole one.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Sync the directory too, so the rename itself survives a crash.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
