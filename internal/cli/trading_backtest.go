package cli

import (
	"bufio"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/trading/backtest"
)

// GitHub #599. The history file comes from broker-ta's history module
// (python -m vornik_broker_ta.history); this command only reads it.
// Design: https://docs.vornik.io

var tradingCmd = &cobra.Command{
	Use:   "trading",
	Short: "Trading measurement tools",
}

var tradingBacktestCmd = &cobra.Command{
	Use:   "backtest",
	Short: "Replay the v2.1 rules over a history file and write the gauntlet report",
	Long: "Replay the ibkr-trader v2.1 entry rules over a daily history file written by\n" +
		"broker-ta's history module, under each exit and gate variant at 1x and 2x\n" +
		"costs, and write a Markdown report: R statistics, walk-forward by year, a\n" +
		"bootstrap, regime splits and a ±10% parameter jitter. Offline: no daemon,\n" +
		"database or broker is contacted.",
	Args: cobra.NoArgs,
	RunE: runTradingBacktest,
}

func init() {
	f := tradingBacktestCmd.Flags()
	f.String("history", "", "history JSONL file from python -m vornik_broker_ta.history (required)")
	f.String("out", "", "Markdown report to write (required)")
	f.Int("bootstrap", 10000, "bootstrap resamples")
	f.Int64("seed", 1, "bootstrap seed")
	f.Bool("skip-jitter", false, "skip the ±10% parameter jitter (it reruns the history ~16 times)")
	_ = tradingBacktestCmd.MarkFlagRequired("history")
	_ = tradingBacktestCmd.MarkFlagRequired("out")
	tradingCmd.AddCommand(tradingBacktestCmd)
	rootCmd.AddCommand(tradingCmd)
}

func runTradingBacktest(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	histPath, _ := f.GetString("history")
	outPath, _ := f.GetString("out")
	n, _ := f.GetInt("bootstrap")
	seed, _ := f.GetInt64("seed")
	skip, _ := f.GetBool("skip-jitter")
	in, err := os.Open(histPath) //nolint:gosec // an operator-named local file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	h, err := backtest.Load(bufio.NewReader(in))
	if err != nil {
		return err
	}
	out, err := os.Create(outPath) //nolint:gosec // an operator-named local file
	if err != nil {
		return err
	}
	if err := backtest.Report(out, h, backtest.Options{Bootstrap: n, Seed: seed, SkipJitter: skip}); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d sessions)\n", outPath, len(h.Sessions))
	return nil
}
