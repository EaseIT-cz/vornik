package service

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
)

// teeForwarder's AppWriter mirrors the EE appWriter's contract
// (internal/enterprise/logship.NewAppWriter): it writes every line through to
// out, unchanged, then offers it to the forwarder (recorded here).
type teeForwarder struct {
	fakeForwarder
	tapMu  sync.Mutex
	tapped []string
}

type teeAppWriter struct {
	out io.Writer
	f   *teeForwarder
}

func (w teeAppWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if err != nil {
		return n, err
	}
	w.f.tapMu.Lock()
	w.f.tapped = append(w.f.tapped, string(p))
	w.f.tapMu.Unlock()
	return n, nil
}

func (f *teeForwarder) AppWriter(out io.Writer) io.Writer {
	f.fakeForwarder.AppWriter(out)
	return teeAppWriter{out: out, f: f}
}

// GitHub #295 (2026-10-08, inferred by lane A from the code): with log
// forwarding on, initLogship re-pointed the root logger at
// MultiWriter(stdout+ring, fwd.AppWriter(stdout)). The app writer already
// passes every line through to the stdout it wraps, so each line reached
// stdout twice. This captures the process's real stdout with logship on and
// asserts every line is written once and offered to the forwarder once.
//
// It swaps the process-global os.Stdout (initLogship writes there directly),
// so it must stay serial: no test in this package calls t.Parallel. It counts
// only its own marker line, so a stray write by another goroutine cannot make
// it pass or fail.
func TestInitLogship_EachLineReachesStdoutOnce(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	fwd := &teeForwarder{}
	c := newTestLogshipContainer(t, ProviderSet{
		Logship: true,
		LogForwarderFactory: func(config.LogForwardConfig, func(string) string) (contracts.LogForwarder, error) {
			return fwd, nil
		},
	})
	c.Logger = zerolog.New(c.rootLogWriter(os.Stdout))
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship: %v", err)
	}
	const marker = "issue-295-marker-line"
	c.Logger.Info().Msg(marker)
	os.Stdout = orig
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Errorf("marker line written to stdout %d times, want 1; stdout:\n%s", got, buf.String())
	}
	fwd.tapMu.Lock()
	offered := strings.Count(strings.Join(fwd.tapped, ""), marker)
	fwd.tapMu.Unlock()
	if offered != 1 {
		t.Errorf("marker line offered to the forwarder %d times, want 1", offered)
	}
}
