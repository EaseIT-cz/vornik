package hermes

import (
	"strings"
	"testing"
)

// Incident (release 2026.10.2 pre-push e2e run, 2026-10-02): TestVornikSide
// failed with "the database system is shutting down". The pgvector image's
// entrypoint initialises the database on a temporary server that listens on
// the Unix socket only, shuts it down, then starts the real server. A socket
// readiness probe answered for the temporary server, and the next psql landed
// during its shutdown. Only the real server listens on TCP, so the probe must
// ask over TCP.
func TestPostgresReadinessProbeAsksOverTCP(t *testing.T) {
	args := postgresReadyArgs("vornik-e2e-pg")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "pg_isready") || !strings.Contains(joined, "-h 127.0.0.1") {
		t.Fatalf("probe %q does not ask over TCP; the init-time server answers on the socket", joined)
	}
	if args[0] != "exec" || args[1] != "vornik-e2e-pg" {
		t.Fatalf("probe %q is not a podman exec into the container", joined)
	}
}
