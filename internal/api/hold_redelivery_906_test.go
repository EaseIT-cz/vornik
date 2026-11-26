package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// GitHub #906: a successful release starts the GitHub redelivery pass in the
// background and answers at once; GET shows the pass running, then its
// report (deploy idle gate design §13.13).

type fakeRedeliveryReport struct {
	State       string `json:"state"`
	Redelivered int    `json:"redelivered"`
}

func TestHoldRedelivery906_ReleaseStartsThePassAndAnswersAtOnce(t *testing.T) {
	rig := newHoldRig(t)
	gate := make(chan struct{})
	got := make(chan RedeliveryWindow, 1)
	WithAfterRelease(func(w RedeliveryWindow) any {
		got <- w
		<-gate // the pass is still running when the DELETE answers
		return fakeRedeliveryReport{State: "done", Redelivered: 3}
	})(rig.s)

	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"redelivery":"started"`) {
		t.Fatalf("release = %d %s, want 200 with redelivery started", w.Code, w.Body.String())
	}
	var win RedeliveryWindow
	select {
	case win = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never started")
	}
	if !win.From.Equal(hold586Since) || !win.To.Equal(hold586Since.Add(time.Minute)) || win.FromProcessStart {
		t.Fatalf("window = %+v, want from the hold's Since to the release time", win)
	}
	if g := holdCall(rig.s, http.MethodGet, drainAdminKey, nil); !strings.Contains(g.Body.String(), `"redelivery":{"state":"running"}`) {
		t.Fatalf("GET while the pass runs = %s, want state running", g.Body.String())
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		g := holdCall(rig.s, http.MethodGet, drainAdminKey, nil)
		var body struct {
			Held       bool                  `json:"held"`
			Redelivery *fakeRedeliveryReport `json:"redelivery"`
		}
		_ = json.Unmarshal(g.Body.Bytes(), &body)
		if body.Redelivery != nil && body.Redelivery.State == "done" {
			if body.Held || body.Redelivery.Redelivered != 3 {
				t.Fatalf("GET after the pass = %s", g.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET never showed the report: %s", g.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for rig.audit.count("daemon.release.redelivery") != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("daemon.release.redelivery rows = %d, want 1", rig.audit.count("daemon.release.redelivery"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHoldRedelivery906_WindowFromProcessStart(t *testing.T) {
	rig := &holdRig{rel: &fakeReleaser{}, audit: &holdAuditRepo{}, timers: &fakeTimers{}, now: hold586Since.Add(time.Minute)}
	info := holdInfo586()
	info.BaseFromStart = true
	rig.s = newHoldServer(rig, info)
	got := make(chan RedeliveryWindow, 1)
	WithAfterRelease(func(w RedeliveryWindow) any { got <- w; return nil })(rig.s)
	if w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil); w.Code != http.StatusOK {
		t.Fatalf("release = %d", w.Code)
	}
	if win := <-got; !win.FromProcessStart {
		t.Fatalf("window = %+v, want FromProcessStart", win)
	}
}

func TestHoldRedelivery906_NoHookIsNotConfigured(t *testing.T) {
	rig := newHoldRig(t)
	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"redelivery":"not_configured"`) {
		t.Fatalf("release = %d %s, want redelivery not_configured", w.Code, w.Body.String())
	}
}

func TestHoldRedelivery906_FailedReleaseNeverRedelivers(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeReleaser)
	}{
		{"reload refused", func(f *fakeReleaser) { f.reloadErr = errors.New("bad yaml") }},
		{"marker not removed", func(f *fakeReleaser) { f.removeErr = errors.New("EPERM") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t)
			tc.set(rig.rel)
			called := make(chan struct{}, 1)
			WithAfterRelease(func(RedeliveryWindow) any { called <- struct{}{}; return nil })(rig.s)
			if w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil); w.Code != http.StatusInternalServerError ||
				strings.Contains(w.Body.String(), "redelivery") {
				t.Fatalf("release = %d %s", w.Code, w.Body.String())
			}
			select {
			case <-called:
				t.Fatal("a failed release started the redelivery pass")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
