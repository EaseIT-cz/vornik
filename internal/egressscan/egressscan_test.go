package egressscan

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/secrets"
)

const awsCanary = "AKIAQWERTYUIOPASDFGH"

func detector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Agent-administered Vornik plan P5: every string leaf of a JSON document is
// scanned and a finding names its path, never its value; credential-shaped
// findings block, heuristic ones do not. Control: ScanJSON and Blocking.
func TestScanJSON(t *testing.T) {
	d := detector(t)
	doc := `{"to":"a@b.example","items":[{"note":"ok"},{"note":"key ` + awsCanary + `"}],"n":5,"nested":{"deep":["x","y"]}}`
	fs, err := ScanJSON(d, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	f, block := Blocking(fs)
	if !block || f.Path != "$.items[1].note" || f.Type != "aws_access_key" {
		t.Fatalf("findings %+v", fs)
	}
	if strings.Contains(f.String(), awsCanary) {
		t.Fatalf("the finding's text carries the value: %s", f.String())
	}
	if fs, _ := ScanJSON(d, []byte(`{"id":"msg-0f8a9c2e7b1d4e6f9a0b3c5d7e9f1a2b","to":"x@y.example"}`)); func() bool { _, b := Blocking(fs); return b }() {
		t.Fatalf("an opaque ID blocked: %+v", fs)
	}
	// Review 20261002-cb24: a canonical key in key=value form still blocks;
	// only the heuristic classes are exempt.
	fs, _ = ScanJSON(d, []byte(`{"note":"aws_access_key_id=`+awsCanary+`"}`))
	if _, b := Blocking(fs); !b {
		t.Fatalf("a canonical key in key=value form did not block: %+v", fs)
	}
	// Not JSON: scanned as one string at $.
	fs, err = ScanJSON(d, []byte("plain "+awsCanary))
	if err != nil || len(fs) == 0 || fs[0].Path != "$" {
		t.Fatalf("non-JSON: %+v %v", fs, err)
	}
	// A key that is itself credential-shaped is found too, and never
	// appears in any path - not even the path of its own value.
	fs, _ = ScanJSON(d, []byte(`{"`+awsCanary+`":"also `+awsCanary+`"}`))
	if _, b := Blocking(fs); !b {
		t.Fatalf("a credential in an object key was missed: %+v", fs)
	}
	for _, f := range fs {
		if strings.Contains(f.Path, awsCanary) {
			t.Fatalf("a path carries the key: %s", f.Path)
		}
	}
}

func TestScanJSON_NilDetectorIsAnError(t *testing.T) {
	if _, err := ScanJSON(nil, []byte(`{}`)); err == nil {
		t.Fatal("a nil detector scanned nothing and said nothing")
	}
}

// The counters carry the surface, type and action, never a value, and an
// examined-but-clean document still counts.
func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.Observe(SurfaceToolArgs, "p", nil, "detect") // before Attach: no-op
	m.Attach(reg)
	m.Attach(reg)
	m.Observe(SurfaceToolArgs, "p", nil, "detect")
	m.Observe(SurfaceToolArgs, "p", []Finding{{Path: "$.a", Type: "aws_access_key"}}, "block")
	if got := testutil.ToFloat64(m.examined.WithLabelValues(SurfaceToolArgs)); got != 2 {
		t.Fatalf("examined = %v", got)
	}
	if got := testutil.ToFloat64(m.findings.WithLabelValues(SurfaceToolArgs, "p", "aws_access_key", "block")); got != 1 {
		t.Fatalf("findings = %v", got)
	}
	var nilM *Metrics
	nilM.Observe(SurfaceToolArgs, "p", nil, "detect")
}
