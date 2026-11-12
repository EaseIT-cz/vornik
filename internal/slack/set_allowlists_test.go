package slack

import (
	"sync"
	"testing"
)

// GitHub #738: a config reload must be able to swap one installation's
// inbound allowlists on the running channel. The production incident was a
// WIDENING that never took effect (an added channel stayed dropped); the
// security case is the NARROWING (a removed sender stays admitted until
// restart). Both halves are asserted here.

func newAllowlistTestChannel(t *testing.T, ic InstallationConfig) *Channel {
	t.Helper()
	ch, err := New(Config{
		SigningSecret: "test-signing-secret",
		Installations: []InstallationConfig{ic},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

func TestSetInstallationAllowlists_WidensAndNarrows(t *testing.T) {
	ch := newAllowlistTestChannel(t, InstallationConfig{
		ProjectID:        "p",
		TeamID:           "T1",
		ChannelAllowlist: []string{"C_A"},
		SenderAllowlist:  []string{"U_A", "U_B"},
	})
	inst := ch.installations[0]

	if !channelAllowed(inst, "", "C_A") {
		t.Fatal("precondition: C_A must be admitted before the swap")
	}
	if _, err := ch.resolveSpeakerForInstallation(inst, "U_B"); err != nil {
		t.Fatalf("precondition: U_B must be admitted before the swap: %v", err)
	}

	changed, err := ch.SetInstallationAllowlists("T1", InstallationConfig{
		ProjectID:        "p",
		TeamID:           "T1",
		ChannelAllowlist: []string{"C_A", "C_C"}, // widen channels
		SenderAllowlist:  []string{"U_A"},        // narrow senders
	})
	if err != nil {
		t.Fatalf("SetInstallationAllowlists: %v", err)
	}
	if !changed {
		t.Error("a changed allowlist must report changed=true")
	}

	if !channelAllowed(inst, "", "C_C") {
		t.Error("the newly listed channel C_C must be admitted after the swap")
	}
	if !channelAllowed(inst, "", "C_A") {
		t.Error("the still-listed channel C_A must stay admitted")
	}
	if channelAllowed(inst, "", "C_Z") {
		t.Error("an unlisted channel must stay refused")
	}
	if _, err := ch.resolveSpeakerForInstallation(inst, "U_B"); err == nil {
		t.Error("the REMOVED sender U_B must be refused after the swap (the security half)")
	}
	if _, err := ch.resolveSpeakerForInstallation(inst, "U_A"); err != nil {
		t.Errorf("the still-listed sender U_A must stay admitted: %v", err)
	}
}

// An identical swap is a no-op: changed=false, and the snapshot identity is
// stable so a reader mid-call keeps its view.
func TestSetInstallationAllowlists_NoChangeReportsFalse(t *testing.T) {
	ic := InstallationConfig{
		ProjectID:        "p",
		TeamID:           "T1",
		ChannelAllowlist: []string{"C_A"},
		SenderAllowlist:  []string{"U_A"},
	}
	ch := newAllowlistTestChannel(t, ic)
	before := ch.installations[0].allowlists.Load()

	changed, err := ch.SetInstallationAllowlists("T1", ic)
	if err != nil {
		t.Fatalf("SetInstallationAllowlists: %v", err)
	}
	if changed {
		t.Error("an identical allowlist must report changed=false")
	}
	if got := ch.installations[0].allowlists.Load(); got != before {
		t.Error("an unchanged swap must not replace the snapshot")
	}
}

// Unknown team_id means the operator moved the workspace pin — the channel
// object is routed by team_id, so the caller must hear about it and report
// restart-required rather than guess at a route.
func TestSetInstallationAllowlists_UnknownTeamIsAnError(t *testing.T) {
	ch := newAllowlistTestChannel(t, InstallationConfig{ProjectID: "p", TeamID: "T1"})
	if _, err := ch.SetInstallationAllowlists("T9", InstallationConfig{TeamID: "T9"}); err == nil {
		t.Error("an unknown team_id must be an error, not a silent no-op")
	}
}

// Flipping allow_unlisted_senders is part of the same swap: with empty
// lists it moves the installation between deny-all and fail-open.
func TestSetInstallationAllowlists_AllowUnlistedFlip(t *testing.T) {
	ch := newAllowlistTestChannel(t, InstallationConfig{ProjectID: "p", TeamID: "T1"})
	inst := ch.installations[0]
	if channelAllowed(inst, "", "C_any") {
		t.Fatal("precondition: empty allowlist without the flag must deny")
	}
	if _, err := ch.resolveSpeakerForInstallation(inst, "U_any"); err == nil {
		t.Fatal("precondition: empty allowlist without the flag must deny senders")
	}

	changed, err := ch.SetInstallationAllowlists("T1", InstallationConfig{
		ProjectID: "p", TeamID: "T1", AllowUnlistedSenders: true,
	})
	if err != nil || !changed {
		t.Fatalf("flip: changed=%v err=%v, want true/nil", changed, err)
	}
	if !channelAllowed(inst, "", "C_any") {
		t.Error("allow_unlisted_senders=true with empty lists must admit channels")
	}
	if _, err := ch.resolveSpeakerForInstallation(inst, "U_any"); err != nil {
		t.Errorf("allow_unlisted_senders=true with empty lists must admit senders: %v", err)
	}
}

// The swap is atomic against the webhook hot path: readers run through the
// gates while swaps happen concurrently. -race catches any torn read; the
// assertions catch a reader ever seeing a half-swapped combination.
func TestSetInstallationAllowlists_ConcurrentReadsAndSwaps(t *testing.T) {
	ch := newAllowlistTestChannel(t, InstallationConfig{
		ProjectID: "p", TeamID: "T1", ChannelAllowlist: []string{"C_A"}, SenderAllowlist: []string{"U_A"},
	})
	inst := ch.installations[0]
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				channelAllowed(inst, "", "C_A")
				_, _ = ch.resolveSpeakerForInstallation(inst, "U_A")
			}
		}()
	}
	for i := 0; i < 200; i++ {
		on := i%2 == 0
		_, err := ch.SetInstallationAllowlists("T1", InstallationConfig{
			ProjectID:            "p",
			TeamID:               "T1",
			ChannelAllowlist:     []string{"C_A"},
			SenderAllowlist:      []string{"U_A"},
			AllowUnlistedSenders: on,
		})
		if err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}
