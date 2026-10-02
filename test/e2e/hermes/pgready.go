package hermes

// postgresReadyArgs is the podman command that asks whether the lane's
// Postgres container is ready. Over TCP, not the socket: the pgvector image
// initialises on a temporary socket-only server, shuts it down, then starts
// the real one, so a socket answer can come from the server about to stop
// (see TestPostgresReadinessProbeAsksOverTCP).
func postgresReadyArgs(container string) []string {
	return []string{"exec", container, "pg_isready", "-h", "127.0.0.1", "-U", "vornik"}
}
