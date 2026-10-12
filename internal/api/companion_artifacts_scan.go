package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/persistence"
)

// Broker binary egress scan (15-artifact-store.md, "Every served byte is
// scanned", 2026-10-12; audit finding E hardened after the lead's security
// review). The text/binary split decides how bytes are PRESENTED, never
// whether they are scanned: a classification the agent controls (name, MIME,
// a sniffed prefix, an early NUL, UTF-16, gzip) must not be a way around the
// scan. A broker view's binary payload is scanned whole, through several
// decodings, before a download link is minted and again when it is redeemed.

// errEgressScanBusy refuses a scan when every verification slot is taken.
var errEgressScanBusy = errors.New("artifact download capacity reached")

// scanBrokerBinary reads the artifact's full bytes (bounded by the download
// limit) and scans them. It returns an "egress_secret: ..." error on a
// finding, or when no scanner is wired (fail closed).
func (s *Server) scanBrokerBinary(ctx context.Context, a *persistence.Artifact, workflowID string) error {
	if s.artifactOpener == nil {
		return errArtifactUnavailable
	}
	s.artifactSlotsInit()
	select {
	case s.artifactDownloads.slots <- struct{}{}:
		defer func() { <-s.artifactDownloads.slots }()
	default:
		return errEgressScanBusy
	}
	r, err := s.artifactOpener.Open(ctx, a.ID)
	if err != nil {
		return errArtifactUnavailable
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, artifactDownloadLimit+1))
	if err != nil || len(b) > artifactDownloadLimit {
		return errArtifactUnavailable
	}
	return s.scanBrokerBytes(a.ProjectID, workflowID, b)
}

// scanBrokerBytes scans every view of b; the first finding refuses.
func (s *Server) scanBrokerBytes(projectID, workflowID string, b []byte) error {
	if s.egress == nil || s.egress.Detector == nil {
		return errors.New("egress_secret: the egress secret scan is not available")
	}
	views, err := binaryScanViews(b)
	if err != nil {
		return err
	}
	for _, v := range views {
		if v == "" {
			continue
		}
		doc, err := json.Marshal(v)
		if err != nil {
			return errArtifactUnavailable
		}
		if why := s.egress.scanAgentDoc(egressscan.SurfaceEgressDoc, projectID, workflowID, doc); why != "" {
			return fmt.Errorf("egress_secret: %s", why)
		}
	}
	return nil
}

// errGzipUnscannable refuses a file that starts like gzip but whose content
// cannot be scanned whole: corrupt, truncated, or larger once decompressed
// than the scan window (review-20261012-02e9 G1, G2). The recipient could
// decompress what the scan did not see, so it fails closed.
var errGzipUnscannable = errors.New("egress_secret: a gzip stream could not be scanned whole (corrupt, or over 32 MiB decompressed)")

// binaryScanViews renders b as the texts a reader could recover from it, the
// way strings(1) does: runs of at least minScanRun printable ASCII
// characters, one per line, from the bytes read as single bytes and as the
// four UTF-16 alignments (LE/BE at offsets 0 and 1, BOM or not), and the same
// for the content of a gzip stream that starts the file (one level). Such a
// stream must decompress cleanly and to at most the download limit, or the
// file is refused (errGzipUnscannable). Credential formats are ASCII and
// longer than the run floor, so a secret after a binary prefix, an early NUL
// or inside UTF-16 text is still a line of its own; dropping short runs keeps
// a scan of a 32 MiB binary affordable (random bytes are mostly short runs).
func binaryScanViews(b []byte) ([]string, error) {
	views := byteViews(b)
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, errGzipUnscannable
		}
		defer func() { _ = zr.Close() }()
		// Multistream: ReadAll advances through every gzip member.
		inner, err := io.ReadAll(io.LimitReader(zr, artifactDownloadLimit+1))
		if err != nil || len(inner) > artifactDownloadLimit {
			return nil, errGzipUnscannable
		}
		views = append(views, byteViews(inner)...)
	}
	return views, nil
}

// minScanRun is the shortest printable run kept (strings(1)'s default).
const minScanRun = 4

func byteViews(b []byte) []string {
	views := []string{asciiRuns(len(b), func(i int) uint16 { return uint16(b[i]) })}
	for off := 0; off < 2; off++ {
		o := off
		n := (len(b) - o) / 2
		if n <= 0 {
			continue
		}
		views = append(views,
			asciiRuns(n, func(i int) uint16 { return uint16(b[o+2*i]) | uint16(b[o+2*i+1])<<8 }),
			asciiRuns(n, func(i int) uint16 { return uint16(b[o+2*i])<<8 | uint16(b[o+2*i+1]) }))
	}
	return views
}

// asciiRuns joins the runs of at least minScanRun printable ASCII units of a
// sequence of n units, one run per line.
func asciiRuns(n int, at func(int) uint16) string {
	var sb strings.Builder
	start := -1
	flush := func(end int) {
		if start >= 0 && end-start >= minScanRun {
			for k := start; k < end; k++ {
				sb.WriteByte(byte(at(k)))
			}
			sb.WriteByte('\n')
		}
		start = -1
	}
	for i := 0; i < n; i++ {
		if u := at(i); (u >= 0x20 && u < 0x7f) || u == '\t' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(n)
	return sb.String()
}

// artifactSlotsInit initialises the signer and the verification slots once.
func (s *Server) artifactSlotsInit() {
	s.artifactDownloads.once.Do(func() {
		s.artifactDownloads.slots = make(chan struct{}, 4)
		_, s.artifactDownloads.err = rand.Read(s.artifactDownloads.key[:])
	})
}
