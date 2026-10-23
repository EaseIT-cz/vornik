// Package subjectstore is the ONE implementation of the GDPR data-subject
// store, the rights ledger and the Art 18 restriction record, on both drivers.
//
// It was postgres.DataSubjectRepository until GitHub #355 (2026-10-13), when the
// SQLite driver gained the tables. One implementation rather than two because
// the repository carries safety checks — the closed collector set, the refusal
// to delete an artifact or a channel session as a plain row, the report stored
// with its hash — and a safety check with two implementations has one that is
// wrong. What differs between the drivers is confined to Dialect.
//
// SQLite opens with foreign_keys(OFF), so the referential rules the Postgres
// schema states as foreign keys (an identifier, link, request or restriction
// needs its parent; deleting a subject cascades to its identifiers and links and
// is refused while the ledger names it) are stated by the statements here.
// Postgres keeps its foreign keys as a second line.
//
// see LLD § https://docs.vornik.io §4.19
package subjectstore

import (
	"errors"
	"time"
)

// Dialect is everything that differs between the drivers. Both statements and
// semantics are otherwise shared, and pinned by the repotest suites on both.
type Dialect struct {
	// Name labels errors ("postgres", "sqlite").
	Name string
	// BindTime renders a timestamp as a query argument. Postgres binds the
	// time.Time; SQLite binds its one fixed-width text form, because it compares
	// timestamps as strings (https://docs.vornik.io).
	BindTime func(time.Time) any
	// ParseTime reads a scanned timestamp column (nil → zero time).
	ParseTime func(src any) (time.Time, error)
	// MapErr maps a driver error to the persistence sentinels: no rows →
	// ErrNotFound, unique violation → ErrDuplicateKey.
	MapErr func(error) error
	// EmbedCacheKey, when set, says the driver has an embedding_cache table an
	// erased memory chunk's vector must be evicted from, and computes the key
	// the embed used (memory.EmbedInputHash: the hash of the contextualised
	// input). Nil on SQLite: no embedder runs there, so there is no cache.
	// A function rather than an import because internal/memory's tests import
	// the SQLite package, which imports this one.
	EmbedCacheKey func(sourceName, content string) string
	// ChannelSessions reports whether the driver stores channel sessions. The
	// SQLite ChannelSessionRepository is a stub (sessions live in process
	// memory), so a channel_sessions link there names a row that does not exist.
	ChannelSessions bool
	// TraceCache reports whether the driver has the blackbox_trace_cache table
	// an erasure evicts (Postgres, migration 68). False on SQLite, which has no
	// such table; there EvictAssembledTraces reports "not applicable".
	TraceCache bool
}

func (d Dialect) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if d.MapErr == nil {
		return err
	}
	return d.MapErr(err)
}

func (d Dialect) bindTime(t time.Time) any {
	if d.BindTime == nil {
		return t
	}
	return d.BindTime(t)
}

// bindTimeOrNull binds a zero time as NULL.
func (d Dialect) bindTimeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return d.bindTime(t)
}

// timeCol is a sql.Scanner for a timestamp column on either driver.
type timeCol struct {
	d *Dialect
	t *time.Time
}

func (c timeCol) Scan(src any) error {
	if src == nil {
		*c.t = time.Time{}
		return nil
	}
	if v, ok := src.(time.Time); ok {
		*c.t = v
		return nil
	}
	if c.d.ParseTime == nil {
		return errors.New("subjectstore: no time parser for a non-time column value")
	}
	t, err := c.d.ParseTime(src)
	if err != nil {
		return err
	}
	*c.t = t
	return nil
}

func (d *Dialect) scanTime(dst *time.Time) timeCol { return timeCol{d: d, t: dst} }

// ErrSubjectRetained is returned by DeleteSubject while a rights request or an
// Art 18 restriction names the subject: the ledger must keep resolving to the
// person it concerns (design §4.8), so the subject outlives its index.
var ErrSubjectRetained = errors.New("subjectstore: subject is named by a rights request or restriction and must be retained")
