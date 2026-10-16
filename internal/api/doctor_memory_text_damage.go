package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/memory"
)

// memoryTextDamageBudget bounds the sweep inside the doctor's 30-second
// request (memory-ingest-paths.md §8.2e). Measured 2026-10-16: 129 MB of chunk
// text; the equivalent Postgres regex scan took 5.1 s.
const memoryTextDamageBudget = 10 * time.Second

// memoryTextDamageMaxItems caps the live chunk ids listed in the verdict.
const memoryTextDamageMaxItems = 20

// memoryTextDamageScan is what one sweep counted.
type memoryTextDamageScan struct {
	Total    int // rows in project_memory_chunks when the sweep started
	Examined int // rows actually read
	Damaged  int // examined rows whose content is damaged text
	Live     int // damaged rows still recallable: not refuted and published
	FFFD     int // examined rows carrying U+FFFD (information, not damage)
	LiveIDs  []string
}

// checkMemoryTextDamage is the operator sweep for GitHub #389: chunks whose
// stored text holds C0 control bytes (other than TAB, LF, CR, FF) or invalid
// UTF-8 — the signature of an upload damaged before it was stored.
//
// The predicate runs in Go (memory.ScanTextDamage), the same one the Indexer
// refuses with, so both drivers give the same answer. Stored text is
// post-chunker on both, so in practice only the control-byte branch fires
// here; U+FFFD is counted and reported as information.
//
// The denominator is published on every path (Finding D of the 2026-08-26
// silent-controls audit): "0 damaged" must never read like "0 examined".
func (h *DoctorHandlers) checkMemoryTextDamage(ctx context.Context) DoctorCheck {
	const name = "memory_text_damage"
	if h.db == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database; cannot sweep memory chunks"}
	}
	sctx, cancel := context.WithTimeout(ctx, memoryTextDamageBudget)
	defer cancel()

	var scan memoryTextDamageScan
	if err := h.db.QueryRowContext(sctx, `SELECT COUNT(*) FROM project_memory_chunks`).Scan(&scan.Total); err != nil {
		if sctx.Err() != nil {
			return evaluateMemoryTextDamage(scan, true)
		}
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: fmt.Sprintf("cannot count memory chunks: %v", err)}
	}

	rows, err := h.db.QueryContext(sctx, `
		SELECT id, project_id, validation_status, lifecycle_state, content
		  FROM project_memory_chunks`)
	if err != nil {
		if sctx.Err() != nil {
			return evaluateMemoryTextDamage(scan, true)
		}
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, projectID, status, lifecycle, content string
		if err := rows.Scan(&id, &projectID, &status, &lifecycle, &content); err != nil {
			if sctx.Err() != nil {
				return evaluateMemoryTextDamage(scan, true)
			}
			return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("scan failed after %d of %d rows: %v", scan.Examined, scan.Total, err)}
		}
		scan.Examined++
		if strings.ContainsRune(content, '�') { // U+FFFD, the chunker's repair mark
			scan.FFFD++
		}
		if !memory.ScanTextDamage(content).Damaged() {
			continue
		}
		scan.Damaged++
		if status == "refuted" || lifecycle != "published" {
			continue
		}
		scan.Live++
		if len(scan.LiveIDs) < memoryTextDamageMaxItems {
			scan.LiveIDs = append(scan.LiveIDs, id+" ("+projectID+")")
		}
	}
	if err := rows.Err(); err != nil {
		if sctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return evaluateMemoryTextDamage(scan, true)
		}
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("read failed after %d of %d rows: %v", scan.Examined, scan.Total, err)}
	}
	return evaluateMemoryTextDamage(scan, false)
}

// evaluateMemoryTextDamage turns a sweep into a verdict. Split from the query
// so the contract is testable without a database.
func evaluateMemoryTextDamage(s memoryTextDamageScan, partial bool) DoctorCheck {
	const name = "memory_text_damage"
	if partial {
		// An unfinished sweep is never OK: what was not read was not examined.
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("sweep did not finish: examined %d of %d chunks before the deadline; %d damaged so far (%d live)", s.Examined, s.Total, s.Damaged, s.Live),
		}
	}
	if s.Examined == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "0 chunks in project_memory_chunks; nothing to examine"}
	}
	summary := fmt.Sprintf("%d of %d chunks examined hold damaged text (%d live, %d refuted or not published); %d carry U+FFFD, the chunker's repair mark (information, not counted as damage)",
		s.Damaged, s.Examined, s.Live, s.Damaged-s.Live, s.FFFD)
	if s.Live == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: summary}
	}
	items := append([]string{}, s.LiveIDs...)
	if s.Live > len(s.LiveIDs) {
		items = append(items, fmt.Sprintf("… and %d more", s.Live-len(s.LiveIDs)))
	}
	items = append(items, "Refute each by id (memory_correct with chunk_ids); the text came from a damaged upload (memory-ingest-paths.md §8). Re-send the source with a tool-encoded upload.")
	return DoctorCheck{Name: name, Status: "WARNING", Message: summary, Items: items}
}
