package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// enterCredential is the credential_slot value entry (design §8.2; plan
// P4.2). Decide first, then store: the value rides the decision's context
// into the slot's effect (slotEffect), which stores it. A refused decision
// stores nothing; a store that fails after the decision is a permanent
// effect failure, recorded on the request by the device service (review
// 20261002-a5d8 F3: before this, the effect was a no-op that marked the
// request applied, so a later failure could not be recorded). The value is
// never logged and never placed in an error.
func (s *agentAdminService) enterCredential(ctx context.Context, d *approverdevice.Device, r persistence.AgentApprovalRequestRow, shownSHA string, value []byte) error {
	if r.Kind != persistence.ApprovalKindCredentialSlot {
		return approverdevice.ErrNotDecidable
	}
	var pl approvalPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.Slot == nil || pl.Slot.Namespace != r.Namespace || pl.Slot.Kind != agentadmin.CredentialSecret {
		return approverdevice.ErrNotDecidable // an OAuth slot is filled by its sign-in only
	}
	if len(value) == 0 || len(value) > approverdevice.MaxValueBytes {
		return approverdevice.ErrNotDecidable
	}
	err := s.devices.Decide(context.WithValue(ctx, enteredValueKey{}, value), d, r.ID, shownSHA, true)
	if errors.Is(err, approverdevice.ErrPermanent) {
		return approverdevice.ErrValueNotStored
	}
	if err != nil {
		return err
	}
	// Now reachable: list the servers that waited for this credential and
	// file their tools approval (plan P4.3). Detached: a listing takes up to
	// listToolsTimeout and the phone is waiting on this page.
	bg := context.WithoutCancel(ctx)
	s.background(func() { s.afterCredentialStored(bg, pl.Slot.Namespace, pl.Slot.Name) })
	return nil
}

// enteredValueKey carries a typed value from the page into the slot effect.
type enteredValueKey struct{}

// oauthCompletionKey carries a sign-in's callback into the slot effect.
type oauthCompletionKey struct{}

type oauthCompletion struct {
	complete func(ctx context.Context) error
}

// slotEffect is the credential_slot effect: it stores what the decision
// carried. Without it (a re-apply after a crash: the value was never
// persisted, by design) the request fails with "ask again".
func (s *agentAdminService) slotEffect(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	var pl approvalPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.Slot == nil {
		return fmt.Errorf("%w: the request does not name its credential", approverdevice.ErrPermanent)
	}
	switch pl.Slot.Kind {
	case agentadmin.CredentialSecret:
		value, ok := ctx.Value(enteredValueKey{}).([]byte)
		if !ok {
			return fmt.Errorf("%w: the value was not received; ask again", approverdevice.ErrPermanent)
		}
		if err := s.storeCredential(ctx, pl, r.DecidedByDevice, value); err != nil {
			s.c.Logger.Error().Str("request_id", r.ID).Str("credential", pl.Slot.Namespace+"/"+pl.Slot.Name).
				Str("cause", errorKind(err)).Msg("agent credential: approved, but the value was not stored")
			return fmt.Errorf("%w: the value could not be stored; ask again", approverdevice.ErrPermanent)
		}
	case agentadmin.CredentialOAuth:
		done, ok := ctx.Value(oauthCompletionKey{}).(oauthCompletion)
		if !ok {
			return fmt.Errorf("%w: the sign-in was not received; ask again", approverdevice.ErrPermanent)
		}
		if err := done.complete(ctx); err != nil {
			s.c.Logger.Error().Err(err).Str("request_id", r.ID).Str("server", pl.Slot.Server).Msg("agent oauth: approved, but the sign-in did not complete")
			return fmt.Errorf("%w: the sign-in did not complete; ask again", approverdevice.ErrPermanent)
		}
	default:
		return fmt.Errorf("%w: unknown credential kind", approverdevice.ErrPermanent)
	}
	return nil
}

func (s *agentAdminService) storeCredential(ctx context.Context, pl approvalPayload, deviceID string, value []byte) error {
	st, err := s.c.secretStoreForWrite()
	if err != nil {
		return err
	}
	return st.Put(ctx, pl.Slot.Namespace, pl.Slot.Name, "secret", value, deviceID)
}

// errorKind names an error's class without its text, which a lower layer
// could have built from the value.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return fmt.Sprintf("%T", err)
	}
}
