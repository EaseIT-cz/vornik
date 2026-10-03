package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbridge"
)

// `vornikctl agent host-approval` (Hermes approval transport design
// https://docs.vornik.io
// §4.3): the Hermes plugin's approval transport runs it with Hermes's
// approval request on stdin. It files the request with the namespace's key
// (the same 0600 file the bridge reads), long-polls until the phone answers
// or the deadline passes, and prints {"choice","reason"}. An answer from the
// phone exits 0; every failure prints deny with its reason and exits
// non-zero, so a missing answer is never an approval. The key never reaches
// stdout, stderr or the plugin.

var (
	hostApprovalNamespace string
	hostApprovalDeadline  float64
	hostApprovalURL       string
)

// errHostApprovalDenied is the non-zero exit; the reason is already on stdout.
var errHostApprovalDenied = errors.New("host approval: denied")

var agentHostApprovalCmd = &cobra.Command{
	Use:   "host-approval",
	Short: "Ask the paired phone to answer an assistant's own safety prompt (run by the assistant, not by you)",
	Long: `Read an approval request from the assistant (Hermes) on stdin, file it with
Vornik under the namespace's key, wait for the paired phone's answer until
--deadline (unix seconds), and print {"choice": "once"|"session"|"deny",
"reason": ...} on stdout.

An answer from the phone exits 0. Anything else (Vornik unreachable, too
many requests, the deadline passed) prints "deny" with the reason and exits
non-zero. The key is never printed.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		deadline := time.Unix(0, int64(hostApprovalDeadline*float64(time.Second)))
		base := hostApprovalURL
		if base == "" {
			base = agentBaseURLFrom("")
		}
		key, err := hostApprovalKey(strings.TrimRight(base, "/"))
		if err != nil {
			printHostChoice(out, "deny", "no key: "+err.Error())
			return errHostApprovalDenied
		}
		// runHostApproval bounds its own context by the deadline.
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if code := runHostApproval(ctx, cmd.InOrStdin(), out, cmd.ErrOrStderr(),
			hostApprovalConfig{Base: strings.TrimRight(base, "/"), Key: key, Deadline: deadline}); code != 0 {
			return errHostApprovalDenied
		}
		return nil
	},
}

func init() {
	agentHostApprovalCmd.Flags().StringVar(&hostApprovalNamespace, "namespace", "", "The agent namespace whose key to use (required)")
	agentHostApprovalCmd.Flags().Float64Var(&hostApprovalDeadline, "deadline", 0, "Unix time (seconds) after which the answer is deny (required)")
	agentHostApprovalCmd.Flags().StringVar(&hostApprovalURL, "url", "", "Vornik's URL (default $VORNIK_API_URL, then "+DefaultAPIURL+")")
	_ = agentHostApprovalCmd.MarkFlagRequired("namespace")
	_ = agentHostApprovalCmd.MarkFlagRequired("deadline")
	agentCmd.AddCommand(agentHostApprovalCmd)
}

// agentBaseURLFrom is flag, then VORNIK_API_URL, then the default.
func agentBaseURLFrom(flag string) string {
	for _, u := range []string{flag, os.Getenv("VORNIK_API_URL")} {
		if u = strings.TrimSpace(u); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return DefaultAPIURL
}

// hostApprovalKey refuses a cleartext endpoint before reading the key,
// then reads it as the bridge does.
func hostApprovalKey(base string) (string, error) {
	if err := agentbridge.CheckEndpoint(base); err != nil {
		return "", err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no config directory: %w", err)
	}
	path, err := agentbridge.KeyPath(dir, hostApprovalNamespace)
	if err != nil {
		return "", err
	}
	return agentbridge.LoadKey(path)
}

// hostApprovalConfig is one run's connection.
type hostApprovalConfig struct {
	Base, Key string
	Deadline  time.Time
	// Poll is the pause between polls that returned pending early (a test
	// seam; the server's long-poll is the normal wait).
	Poll time.Duration
	HTTP *http.Client
}

type hostApprovalAnswer struct {
	Status string `json:"status"`
	Choice string `json:"choice"`
}

func printHostChoice(out io.Writer, choice, reason string) {
	b, _ := json.Marshal(map[string]string{"choice": choice, "reason": reason})
	_, _ = fmt.Fprintln(out, string(b))
}

// runHostApproval files the request on in and waits for its answer. It
// returns the exit code: 0 for an answer from the phone, 1 otherwise.
func runHostApproval(ctx context.Context, in io.Reader, out, errw io.Writer, cfg hostApprovalConfig) int {
	deny := func(reason string) int {
		printHostChoice(out, "deny", reason)
		_, _ = fmt.Fprintf(errw, "vornikctl agent host-approval: deny (%s)\n", reason)
		return 1
	}
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return deny("could not read the request")
	}
	var req struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &req) != nil || req.RequestID == "" {
		return deny("the request on stdin is not an approval request")
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	poll := cfg.Poll
	if poll <= 0 {
		poll = time.Second
	}
	ctx, cancel := context.WithDeadline(ctx, cfg.Deadline)
	defer cancel()

	ans, reason := hostApprovalCall(ctx, hc, cfg, http.MethodPost, "/api/v1/agent/host-approvals", raw)
	for {
		if reason != "" {
			if ctx.Err() != nil {
				return deny("timeout")
			}
			return deny(reason)
		}
		if ans.Status != "pending" {
			choice, why, ok := hostVerdict(ans)
			if !ok {
				return deny(why)
			}
			printHostChoice(out, choice, why)
			return 0
		}
		left := time.Until(cfg.Deadline)
		if left <= 0 {
			return deny("timeout")
		}
		// The server long-polls up to 25 s; under a second left, a plain read
		// after a short pause.
		path := "/api/v1/agent/host-approvals/" + url.PathEscape(req.RequestID)
		if wait := int(min(left, 25*time.Second) / time.Second); wait >= 1 {
			path += fmt.Sprintf("?wait=%d", wait)
		}
		started := time.Now()
		ans, reason = hostApprovalCall(ctx, hc, cfg, http.MethodGet, path, nil)
		if reason == "" && ans.Status == "pending" && time.Since(started) < poll {
			select {
			case <-ctx.Done():
				return deny("timeout")
			case <-time.After(min(poll, left)):
			}
		}
	}
}

// hostVerdict reads a decided state: ok with the choice for an answer from
// the phone, else the reason it is a deny.
func hostVerdict(ans hostApprovalAnswer) (choice, reason string, ok bool) {
	switch {
	case ans.Status == "expired":
		return "", "expired", false
	case ans.Status == "approved" && (ans.Choice == "once" || ans.Choice == "session"):
		return ans.Choice, "answered on the paired phone", true
	case ans.Status == "rejected" && ans.Choice == "deny":
		return "deny", "denied on the paired phone", true
	}
	return "", "invalid answer", false
}

// hostApprovalCall makes one request. reason is "" on a readable answer,
// else why there is none; it never contains the key.
func hostApprovalCall(ctx context.Context, hc *http.Client, cfg hostApprovalConfig, method, path string, body []byte) (hostApprovalAnswer, string) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Base+path, rd)
	if err != nil {
		return hostApprovalAnswer{}, "invalid URL"
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return hostApprovalAnswer{}, "timeout"
		}
		return hostApprovalAnswer{}, "Vornik unreachable"
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return hostApprovalAnswer{}, "busy: " + serverReason(b)
	case http.StatusConflict:
		return hostApprovalAnswer{}, "conflict"
	case http.StatusUnauthorized, http.StatusForbidden:
		return hostApprovalAnswer{}, "refused by Vornik (is this namespace still connected?)"
	case http.StatusNotFound:
		return hostApprovalAnswer{}, "not found (this Vornik may be too old for host approvals)"
	default:
		return hostApprovalAnswer{}, fmt.Sprintf("Vornik answered %d", resp.StatusCode)
	}
	var a hostApprovalAnswer
	if json.Unmarshal(b, &a) != nil {
		return hostApprovalAnswer{}, "invalid answer"
	}
	return a, ""
}

// serverReason is the reason field of an error body, bounded.
func serverReason(b []byte) string {
	var e struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(b, &e) != nil || e.Reason == "" {
		return "too many requests"
	}
	if len(e.Reason) > 200 {
		return e.Reason[:200]
	}
	return e.Reason
}
