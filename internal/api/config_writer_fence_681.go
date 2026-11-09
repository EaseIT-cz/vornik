package api

import (
	"errors"
	"net/http"

	"vornik.io/vornik/internal/configlock"
)

// respondNotConfigWriter answers 409 NOT_CONFIG_WRITER when err says this
// daemon does not hold the config_writer lease (GitHub #681,
// operations-api-contract design §6.10), and reports whether it answered.
// Nothing was written: the refusal comes before the read.
func respondNotConfigWriter(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, configlock.ErrNotConfigWriter) {
		return false
	}
	respondError(w, http.StatusConflict, "NOT_CONFIG_WRITER",
		"this daemon is not the config writer; make this change on the daemon that holds the config_writer lease")
	return true
}
