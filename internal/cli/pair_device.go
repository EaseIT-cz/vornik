package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl pair-device` and `vornikctl devices list|revoke` (agent-
// administered Vornik design §9.2, plan P2.5).
//
// They open the daemon's storage DIRECTLY with the daemon's config, the
// `vornikctl broker-action` pattern, and there is deliberately no HTTP route
// for them. Pairing therefore proves possession of the account that can read
// the daemon's config and database: when Vornik runs as its own OS user, that
// is the Vornik account, which a harness running as the human's user cannot
// use. An HTTP admin route would instead make "holds an admin-class key" the
// pairing factor, and a key in the human's environment is usable by an agent
// running as that human.

var (
	pairDeviceLabel string
	devicesListJSON bool
)

var pairDeviceCmd = &cobra.Command{
	Use:   "pair-device",
	Short: "Pair a phone as an approver device (prints a one-time code)",
	Long: `Print a one-time code (8 characters, 10 minutes, single use) to pair a phone
as an approver device. Open the printed address on the phone and enter the
code.

Only an approver device can approve what an agent asks Vornik to do, or enter
a credential for it. The first device is paired by the code alone; once a
device exists, a new one must also be approved on an existing device.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc, cfg, closeFn, err := openApproverDevices(ctx)
		if err != nil {
			return err
		}
		defer closeFn()
		return pairDevice(ctx, svc, cfg.PublicOrigin(), pushConfigured(cfg), pairDeviceLabel, cmd.OutOrStdout())
	},
}

var devicesCmd = &cobra.Command{
	Use:   "devices",
	Short: "List or revoke approver devices",
}

var devicesListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List approver devices, revoked ones included",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc, _, closeFn, err := openApproverDevices(ctx)
		if err != nil {
			return err
		}
		defer closeFn()
		return listDevices(ctx, svc, devicesListJSON, cmd.OutOrStdout())
	},
}

var devicesRevokeCmd = &cobra.Command{
	Use:          "revoke <device_id>",
	Short:        "Revoke an approver device",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc, _, closeFn, err := openApproverDevices(ctx)
		if err != nil {
			return err
		}
		defer closeFn()
		return revokeDevice(ctx, svc, args[0], cmd.OutOrStdout())
	},
}

func init() {
	pairDeviceCmd.Flags().StringVar(&pairDeviceLabel, "label", "", "A name for the device (1 to 40 characters), shown in approvals and alerts (default: \"Phone paired <date time>\")")
	devicesListCmd.Flags().BoolVar(&devicesListJSON, "json", false, "Print JSON")
	devicesCmd.AddCommand(devicesListCmd, devicesRevokeCmd)
	rootCmd.AddCommand(pairDeviceCmd, devicesCmd)
}

func openApproverDevices(ctx context.Context) (*approverdevice.Service, *config.Config, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open the daemon's database (pair-device runs as the account that runs Vornik): %w", err)
	}
	if backend.Repos == nil || backend.Repos.ApproverDevices == nil {
		_ = backend.Close()
		return nil, nil, nil, errors.New("this daemon's storage has no approver-device store")
	}
	return approverdevice.New(backend.Repos.ApproverDevices), cfg, func() { _ = backend.Close() }, nil
}

// pushConfigured is the daemon's own predicate (§9.3), so the warning here
// and the approver_devices doctor check agree.
func pushConfigured(cfg *config.Config) bool { return cfg.OperatorAlertActive() }

func pairDevice(ctx context.Context, svc *approverdevice.Service, origin string, push bool, label string, out io.Writer) error {
	exists, err := svc.DeviceExists(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" {
		label = defaultDeviceLabel(nowFunc())
	}
	code, expires, err := svc.StartPairing(ctx, label)
	if err != nil {
		return err
	}
	shown := code
	if len(code) == 8 {
		shown = code[:4] + "-" + code[4:]
	}
	if origin == "" {
		origin = "https://<this host>"
	}
	_, _ = fmt.Fprintf(out, "Pairing code:  %s\n", shown)
	_, _ = fmt.Fprintf(out, "Open on the phone:  %s/ui/pair\n", origin)
	_, _ = fmt.Fprintf(out, "Expires:  %s (single use)\n", expires.Local().Format("15:04:05 MST"))
	if plainHTTPNonLoopback(origin) {
		_, _ = fmt.Fprintln(out, "Warning: pairing needs HTTPS. The approver pages refuse plain http to a non-loopback host: serve Vornik over HTTPS (or behind a TLS proxy listed in server.real_ip.trusted_proxies), or pair on this machine through localhost.")
	}
	if exists {
		_, _ = fmt.Fprintln(out, "An approver device already exists: after the code, approve this one on that device.")
	}
	if !push {
		_, _ = fmt.Fprintln(out, "Warning: no alert channel is configured (steering_operator_alert), so pairings and approval requests are not pushed. Approvals still work from the page.")
	}
	return nil
}

// plainHTTPNonLoopback reports whether origin is an http URL whose host is not
// the local machine (design §9.2, T12: the approver pages refuse it).
func plainHTTPNonLoopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	h := strings.TrimSuffix(u.Hostname(), ".")
	if strings.EqualFold(h, "localhost") {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

type deviceListRow struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	PairedAt   time.Time  `json:"paired_at"`
	PairedBy   string     `json:"paired_by"`
	LastUsedAt time.Time  `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// listDevices prints devices. It never prints a token or its hash.
func listDevices(ctx context.Context, svc *approverdevice.Service, asJSON bool, out io.Writer) error {
	devs, err := svc.ListDevices(ctx)
	if err != nil {
		return err
	}
	rows := make([]deviceListRow, 0, len(devs))
	for _, d := range devs {
		rows = append(rows, deviceListRow{ID: d.ID, Label: d.Label, PairedAt: d.PairedAt, PairedBy: d.PairedBy, LastUsedAt: d.LastUsedAt, RevokedAt: d.RevokedAt})
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No approver devices. Pair one with: vornikctl pair-device")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tLABEL\tPAIRED\tBY\tLAST USED\tSTATE")
	for _, r := range rows {
		state := "active"
		if r.RevokedAt != nil {
			state = "revoked " + r.RevokedAt.UTC().Format("2006-01-02")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Label, r.PairedAt.UTC().Format("2006-01-02 15:04"),
			r.PairedBy, r.LastUsedAt.UTC().Format("2006-01-02 15:04"), state)
	}
	return tw.Flush()
}

func revokeDevice(ctx context.Context, svc *approverdevice.Service, id string, out io.Writer) error {
	devs, err := svc.ListDevices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.ID == id {
			if err := svc.Revoke(ctx, id); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "Revoked %s (%s).\n", id, d.Label)
			return nil
		}
	}
	return fmt.Errorf("no approver device %q (see: vornikctl devices list)", id)
}

// nowFunc is the clock for the default device label; tests replace it.
var nowFunc = time.Now

// defaultDeviceLabel names a device by when it was paired, so devices paired
// without --label differ (GitHub #79). At most 40 runes; passes CleanLabel.
func defaultDeviceLabel(now time.Time) string {
	return "Phone paired " + now.Format("2 Jan 15:04")
}
