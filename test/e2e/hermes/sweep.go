package hermes

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sweep is the canary sweep of agent-administered Vornik's DoD lane (plan
// P8.5). Each place is something a harness received (ReachesHarness) or a
// trace inside Vornik. The report says how much each place examined, not
// only what it found, so a place that examined nothing fails instead of
// passing as clean (CLAUDE.md §4).
type Sweep struct {
	creds, raw []string
	places     []*sweepPlace
}

type sweepPlace struct {
	name    string
	harness bool
	items   int
	bytes   int
	hits    map[string]int
	// emptyWhy, when set, is why the place holds nothing in this
	// configuration: it must then be empty, and an empty place passes.
	emptyWhy string
}

// NewSweep searches for the credential and raw-record canaries.
func NewSweep(credentialCanaries, rawCanaries []string) *Sweep {
	return &Sweep{creds: credentialCanaries, raw: rawCanaries}
}

func (s *Sweep) place(name string, harness bool) *sweepPlace {
	p := &sweepPlace{name: name, harness: harness, hits: map[string]int{}}
	s.places = append(s.places, p)
	return p
}

func (s *Sweep) scan(p *sweepPlace, text string) {
	p.items++
	p.bytes += len(text)
	for _, c := range append(append([]string{}, s.creds...), s.raw...) {
		if n := strings.Count(text, c); n > 0 {
			p.hits[c] += n
		}
	}
}

// AddText sweeps one text (a transcript, a log).
func (s *Sweep) AddText(name string, harness bool, text string) {
	p := s.place(name, harness)
	if text == "" {
		return
	}
	s.scan(p, text)
}

// AddRows sweeps database text, one item per row.
func (s *Sweep) AddRows(name string, harness bool, rows []string) {
	p := s.place(name, harness)
	for _, r := range rows {
		s.scan(p, r)
	}
}

// AddRowsMustBeEmpty sweeps a table this configuration never writes, saying
// why. It passes only while it holds nothing; a row in it is swept like any
// other, so a configuration change that starts writing it is examined, not
// skipped.
func (s *Sweep) AddRowsMustBeEmpty(name, why string, rows []string) {
	p := s.place(name, false)
	p.emptyWhy = why
	for _, r := range rows {
		s.scan(p, r)
	}
}

// AddDir sweeps every regular file under dir.
func (s *Sweep) AddDir(name string, harness bool, dir string) {
	p := s.place(name, harness)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil {
			s.scan(p, string(b))
		}
		return nil
	})
}

// Violations is the gate: a canary in anything that reached a harness, a
// credential canary anywhere, or a place that examined nothing.
func (s *Sweep) Violations() []string {
	var out []string
	for _, p := range s.places {
		if p.items == 0 || p.bytes == 0 {
			if p.emptyWhy == "" {
				out = append(out, p.name+": examined nothing")
			}
			continue
		}
		for _, c := range s.creds {
			if p.hits[c] > 0 {
				out = append(out, fmt.Sprintf("%s: credential canary %s found %d times", p.name, c, p.hits[c]))
			}
		}
		if p.harness {
			for _, c := range s.raw {
				if p.hits[c] > 0 {
					out = append(out, fmt.Sprintf("%s: raw-record canary %s reached a harness (%d times)", p.name, c, p.hits[c]))
				}
			}
		}
	}
	return out
}

// Report is one line per place: what it examined and what it found.
func (s *Sweep) Report() string {
	var b strings.Builder
	for _, p := range s.places {
		where := "reached a harness"
		if !p.harness {
			where = "inside Vornik"
		}
		var hits []string
		for c, n := range p.hits {
			hits = append(hits, fmt.Sprintf("%s=%d (%s)", c, n, where))
		}
		sort.Strings(hits)
		if len(hits) == 0 {
			hits = []string{"no canary"}
		}
		if p.items == 0 && p.emptyWhy != "" {
			hits = []string{"empty, as expected: " + p.emptyWhy}
		}
		fmt.Fprintf(&b, "%s: %d items, %d bytes; %s\n", p.name, p.items, p.bytes, strings.Join(hits, ", "))
	}
	return b.String()
}
