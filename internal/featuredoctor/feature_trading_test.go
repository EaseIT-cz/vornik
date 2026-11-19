package featuredoctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTradingProbe is a TradingSeriesProbe returning canned findings/error.
type stubTradingProbe struct {
	findings []TradingSeriesFinding
	err      error
}

func (s stubTradingProbe) ValidateSeries(_ context.Context) (TradingSeriesReport, error) {
	return TradingSeriesReport{Findings: s.findings, LedgerSymbols: 4, LedgerProjects: 1, LedgerWired: true}, s.err
}

func TestTradingFeature_VerifyClean_OK(t *testing.T) {
	f := tradingFeature()
	deps := Deps{Trading: stubTradingProbe{findings: nil}}
	res := f.Verify(context.Background(), deps)
	if !res.OK {
		t.Fatalf("clean series should verify OK, got %+v", res)
	}
}

func TestTradingFeature_VerifyFindings_NotOK(t *testing.T) {
	f := tradingFeature()
	deps := Deps{Trading: stubTradingProbe{findings: []TradingSeriesFinding{
		{ProjectID: "trader", Code: "cadence_gap", Severity: "warn", Detail: "2 gaps"},
		{ProjectID: "trader", Code: "out_of_bounds", Severity: "fail", Detail: "negative equity"},
	}}}
	res := f.Verify(context.Background(), deps)
	if res.OK {
		t.Fatalf("findings should make verify not-OK, got %+v", res)
	}
	if res.Detail == "" || res.Remediation == "" {
		t.Errorf("findings verify should carry Detail + Remediation, got %+v", res)
	}
}

func TestTradingFeature_VerifyNilProbe_OKSkipped(t *testing.T) {
	f := tradingFeature()
	res := f.Verify(context.Background(), Deps{Trading: nil})
	if !res.OK {
		t.Fatalf("nil probe should degrade gracefully to OK/skip, got %+v", res)
	}
}

func TestTradingFeature_VerifyProbeError_NotOK(t *testing.T) {
	f := tradingFeature()
	deps := Deps{Trading: stubTradingProbe{err: errors.New("db down")}}
	res := f.Verify(context.Background(), deps)
	if res.OK {
		t.Fatalf("probe error should make verify not-OK, got %+v", res)
	}
}

// TestTradingFeature_Diagnose maps Verify outcomes to feature status. The
// feature has no gates, so gatesOn is always true and Verify runs.
func TestTradingFeature_Diagnose(t *testing.T) {
	cfg := stubConfig{vals: map[string]any{}}

	clean := Diagnose(context.Background(), tradingFeature(), Deps{
		Config: cfg, Trading: stubTradingProbe{},
	})
	if clean.Status != StatusOK {
		t.Errorf("clean series should diagnose StatusOK, got %s", clean.Status)
	}

	dirty := Diagnose(context.Background(), tradingFeature(), Deps{
		Config: cfg, Trading: stubTradingProbe{findings: []TradingSeriesFinding{
			{ProjectID: "trader", Code: "stale", Severity: "warn", Detail: "x"},
		}},
	})
	if dirty.Status != StatusDegraded {
		t.Errorf("findings should diagnose StatusDegraded, got %s", dirty.Status)
	}
}

// GitHub #822: the clean result states what the ledger comparison examined,
// so "no anomalies" is never read as "never looked".
func TestVerifyTradingSeries_CleanStatesTheLedgerCount_822(t *testing.T) {
	res := verifyTradingSeries(context.Background(), Deps{Trading: stubTradingProbe{}})
	if !res.OK || !strings.Contains(res.Detail, "ledger compared 4 symbols in 1 projects") {
		t.Fatalf("clean verify = %+v, want OK with the ledger count", res)
	}
}

// Review e95c F2: a probe without the ledger's repositories says so.
func TestVerifyTradingSeries_UnwiredLedgerSaysSo_822(t *testing.T) {
	res := verifyTradingSeries(context.Background(), Deps{Trading: unwiredLedgerProbe{}})
	if !strings.Contains(res.Detail, "ledger not compared") {
		t.Fatalf("detail = %q", res.Detail)
	}
}

type unwiredLedgerProbe struct{}

func (unwiredLedgerProbe) ValidateSeries(context.Context) (TradingSeriesReport, error) {
	return TradingSeriesReport{}, nil
}
