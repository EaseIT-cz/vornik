package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/secrets"
)

// EgressScan is Part A of the 2026-07-16 secret-egress design (agent-
// administered Vornik plan P5.1): every daemon-proxied tool call's
// arguments are scanned before dispatch. An agent-namespace project blocks
// on a credential-shaped finding and cannot be configured down; an operator
// project follows its tool_egress checkpoint (Policy; default detect).
type EgressScan struct {
	Detector secrets.Detector
	// Policy returns an operator project's action.
	Policy func(projectID string) secrets.Action
	// Record receives every examined document's surface, findings and the
	// action taken. Optional. Findings carry a path and a type, never the
	// secret value itself.
	Record func(surface, projectID, what string, fs []egressscan.Finding, action secrets.Action)
}

// WithEgressScan wires the egress scan the agent query route and the
// companion result use (plan P5.2, P5.4); the composed executor carries
// its own reference.
func WithEgressScan(e *EgressScan) ServerOption {
	return func(s *Server) { s.egress = e }
}

// scanAgentDoc scans an agent project's outbound document on surface and
// returns the first credential-shaped finding as a refusal, or "". It fails
// closed: no scanner, or a scan error, refuses.
func (e *EgressScan) scanAgentDoc(surface, projectID, what string, doc []byte) string {
	if e == nil || e.Detector == nil {
		return "the egress secret scan is not available"
	}
	fs, err := egressscan.ScanJSON(e.Detector, doc)
	if err != nil {
		return "the egress secret scan failed"
	}
	if e.Record != nil {
		e.Record(surface, projectID, what, fs, secrets.ActionBlock)
	}
	if f, credential := egressscan.Blocking(fs); credential {
		return f.String()
	}
	return ""
}

// ErrEgressScanUnavailable refuses an agent project's call when nothing can
// scan it: agent projects fail closed.
var ErrEgressScanUnavailable = errors.New("tool call refused: the egress secret scan is not available")

// egressCheck returns the arguments to forward (masked under redact) or an
// error refusing the call.
func (c *ComposedMCPExecutor) egressCheck(projectID, tool, argsJSON string) (string, error) {
	_, agent := agentns.FromID(projectID)
	if c.Egress == nil || c.Egress.Detector == nil {
		if agent {
			return "", ErrEgressScanUnavailable
		}
		return argsJSON, nil
	}
	action := secrets.ActionDetect
	if agent {
		action = secrets.ActionBlock
	} else if c.Egress.Policy != nil {
		action = c.Egress.Policy(projectID)
	}
	fs, err := egressscan.ScanJSON(c.Egress.Detector, []byte(argsJSON))
	if err != nil {
		if agent {
			return "", ErrEgressScanUnavailable
		}
		return argsJSON, nil // operator scans fail open (Part A)
	}
	if c.Egress.Record != nil {
		c.Egress.Record(egressscan.SurfaceToolArgs, projectID, tool, fs, action)
	}
	f, credential := egressscan.Blocking(fs)
	if !credential {
		return argsJSON, nil
	}
	switch action {
	case secrets.ActionBlock:
		return "", fmt.Errorf("tool arguments refused: %s; never send a credential in a tool call", f)
	case secrets.ActionRedact:
		masked := secrets.Redact([]byte(argsJSON), c.Egress.Detector.Scan([]byte(argsJSON)))
		if !json.Valid(masked) {
			return "", fmt.Errorf("tool arguments refused: %s, and masking it broke the arguments", f)
		}
		return string(masked), nil
	default:
		return argsJSON, nil
	}
}
