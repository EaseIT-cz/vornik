package approverdevice

// GitHub #669 item 5 (design 22, §22.2.16b): an approved request whose
// change could not be applied is shown on the phone for 7 days, read-only,
// with its reason masked and cut. Before, after the one terminal notice it
// appeared nowhere a person looks (the 2026-10-28 incident). Found at:
// production. Regression test at: integration (the real phone handler and
// templates over the SQLite repository). Lower because there is no browser
// lane.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

const plantedToken669 = "ghp_APPLYFAILEDCANARY0123456789abcdefghi"

func failRequest669(t *testing.T, f *fixture, id, sentence, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindDeviceEnrollment,
		Sentence: sentence, Rendered: []byte(`{"kind":"device_enrollment","label":"x"}`), RenderedSHA256: "h_" + id,
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Decide(ctx, id, "h_"+id, "dev_x", true, f.clock()); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.MarkApplyFailed(ctx, id, reason, f.clock()); err != nil {
		t.Fatal(err)
	}
}

func TestPages669_ApplyFailedIsShownMasked(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	failRequest669(t, f, "apr_f1", "Connect the Hermes account.",
		"gave up after 60 attempts: token "+plantedToken669+" refused; "+strings.Repeat("x", 400))
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	for _, want := range []string{"Could not be applied", "Connect the Hermes account.", "gave up after 60 attempts"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, plantedToken669) {
		t.Error("the failure reason shows a secret unmasked")
	}
	if strings.Contains(list, strings.Repeat("x", 300)) {
		t.Error("the failure reason is not cut to 200 characters")
	}
	if strings.Contains(list, "More requests failed than are shown here.") {
		t.Error("one failure says the list is capped")
	}
}

func TestPages669_ApplyFailedCapIsNotSilent(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	for i := 0; i < 21; i++ {
		failRequest669(t, f, fmt.Sprintf("apr_c%02d", i), fmt.Sprintf("Change number %02d.", i), "busy")
	}
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if n := strings.Count(list, "Change number "); n != 20 {
		t.Errorf("%d failed requests shown, want 20", n)
	}
	if !strings.Contains(list, "More requests failed than are shown here.") {
		t.Error("a capped list does not say so")
	}
}
