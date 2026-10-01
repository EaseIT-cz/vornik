package api

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"vornik.io/vornik/internal/companionpush"
	"vornik.io/vornik/internal/persistence"
)

// Companion completion push — broker write-actions design §7a. delegate
// accepts notify {url, token}; the pusher loop (internal/companionpush)
// sends {task_id, state} and action states to it. Pushes are hints: they
// carry ids and closed states only, and status stays authoritative.

// companionNotify is delegate's optional notify argument.
type companionNotify struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// WithCompanionPushConfigs wires the push-config store. Wire it only where
// the companion pusher runs; companion-push is advertised exactly then.
func WithCompanionPushConfigs(r persistence.A2APushConfigRepository) ServerOption {
	return func(srv *Server) { srv.companionPushConfigs = r }
}

// checkCompanionNotify validates notify before any task is created. A
// refusal names the rule, never echoes the token.
func (s *Server) checkCompanionNotify(key *persistence.APIKey, n *companionNotify) error {
	if n == nil {
		return nil
	}
	if s.companionPushConfigs == nil {
		return fmt.Errorf("NOTIFY_REJECTED: this daemon does not push (capability companion-push is off); poll result instead")
	}
	allowed := s.companionPushAllowed(key.ProjectID)
	if err := companionpush.ValidateNotifyURL(n.URL, allowed); err != nil {
		return fmt.Errorf("NOTIFY_REJECTED: %s", err.Error())
	}
	if err := companionpush.ValidateNotifyToken(n.Token); err != nil {
		return fmt.Errorf("NOTIFY_REJECTED: %s", err.Error())
	}
	return nil
}

// registerCompanionPush stores notify for a created task. The task exists
// either way, so a store failure is reported in the response, not as an
// error: the agent then polls.
func (s *Server) registerCompanionPush(ctx context.Context, taskID string, n *companionNotify) string {
	err := s.companionPushConfigs.Set(ctx, persistence.A2APushConfig{
		TaskID: taskID, URL: strings.TrimSpace(n.URL), Token: n.Token,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("companion push: config not stored; the client must poll")
		return "not_registered"
	}
	return "registered"
}

func (s *Server) companionPushAllowed(projectID string) []netip.Prefix {
	if s.projectRegistry == nil {
		return nil
	}
	return s.projectRegistry.GetProject(projectID).CompanionPushAllowed()
}
