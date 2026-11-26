package api

// GitHub #859: control_plane_restore_reload reports the daemon's last
// unconfirmed config reload, with the remedy for its kind.

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/controlplane"
)

func TestCheckRestoreReload_859(t *testing.T) {
	h := &DoctorHandlers{}
	if c := h.checkRestoreReload(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", c)
	}
	var rec *controlplane.UnconfirmedReload
	h.SetUnconfirmedReload(func() *controlplane.UnconfirmedReload { return rec })
	if c := h.checkRestoreReload(context.Background()); c.Status != "OK" {
		t.Fatalf("no record: %+v", c)
	}
	for kind, remedy := range map[controlplane.ReloadKind]string{
		controlplane.ReloadRejected: "vornikctl config reload",
		controlplane.ReloadTimedOut: "restart the daemon",
		controlplane.ReloadStuck:    "restart the daemon",
	} {
		rec = &controlplane.UnconfirmedReload{Ref: "cpp_1", Kind: kind, Error: "boom", At: time.Now()}
		c := h.checkRestoreReload(context.Background())
		if c.Status != "ERROR" || !strings.HasPrefix(c.Message, "CRITICAL:") || !strings.Contains(c.Message, "cpp_1") || !strings.Contains(c.Message, string(kind)) || !strings.Contains(c.Message, remedy) {
			t.Errorf("%s: %+v", kind, c)
		}
	}
}
