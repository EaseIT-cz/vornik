package ui

import (
	"net/http"
)

// refuseNotConfigWriter answers 409 when this daemon does not hold the
// config_writer lease, so a UI save never writes the shared config tree from a
// daemon that is not its writer (GitHub #681, operations-api-contract design
// §6.10). Nothing was written: the refusal comes before the read.
func (s *Server) refuseNotConfigWriter(w http.ResponseWriter, err error) {
	if s != nil {
		s.logger.Warn().Err(err).Msg("ui: config write refused: this daemon is not the config writer")
	}
	http.Error(w, "This daemon is not the config writer: make this change on the daemon that holds the config_writer lease. Nothing was saved.", http.StatusConflict)
}
