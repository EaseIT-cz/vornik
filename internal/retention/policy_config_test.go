package retention

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// #334 audit 2026-10-14: three hand-written copies of the config→Policy
// mapping drifted (the CLI lost MemoryEvictionAuditDays). This guards the
// shared builders: every int or bool field present by name on both the
// source struct and Policy is copied. Each source field gets a distinct
// value, so a missed or crossed field shows. The examined count is logged.
func TestPolicyFromConfig_CopiesEveryWindow(t *testing.T) {
	check := func(t *testing.T, src any, build func(any) Policy) {
		t.Helper()
		sv := reflect.New(reflect.TypeOf(src)).Elem()
		want := map[string]any{}
		for i := 0; i < sv.NumField(); i++ {
			f := sv.Type().Field(i)
			if _, ok := reflect.TypeOf(Policy{}).FieldByName(f.Name); !ok {
				continue
			}
			switch f.Type.Kind() {
			case reflect.Int:
				sv.Field(i).SetInt(int64(1000 + i))
				want[f.Name] = 1000 + i
			case reflect.Bool:
				sv.Field(i).SetBool(true)
				want[f.Name] = true
			}
		}
		if len(want) == 0 {
			t.Fatal("no shared field examined")
		}
		got := reflect.ValueOf(build(sv.Interface()))
		for name, w := range want {
			if g := got.FieldByName(name).Interface(); g != w {
				t.Errorf("%s: Policy has %v, source has %v (not copied)", name, g, w)
			}
		}
		t.Logf("examined %d shared fields of %s", len(want), sv.Type().Name())
	}
	t.Run("config", func(t *testing.T) {
		check(t, config.RetentionConfig{}, func(v any) Policy {
			rc := v.(config.RetentionConfig)
			// ToolAuditFloorDays reaches the Policy only through
			// ResolveToolAuditFloor, which the caller passes in.
			return DefaultsFromConfig(rc, rc.ToolAuditFloorDays, "")
		})
	})
	t.Run("project", func(t *testing.T) {
		check(t, registry.ProjectRetention{}, func(v any) Policy { return ProjectOverrides(v.(registry.ProjectRetention)) })
	})
}

// #334 audit 2026-10-14 (found while sharing the builder): Resolve did not
// carry GraphQuarantineDays, so every resolved policy read 0 (the 30-day
// default) and `retention.graph_quarantine_days: -1`, the documented
// legal-hold switch that disables the quarantined-graph purge, did nothing
// on the daemon's sweep or the CLI. Resolve now carries the daemon value
// (there is no per-project override): negative disables, zero is the
// default at the purge, positive is the window.
func TestResolve_CarriesGraphQuarantineDays(t *testing.T) {
	for _, days := range []int{-1, 0, 45} {
		got := Resolve("p", ProjectOverrides(registry.ProjectRetention{}), DefaultsFromConfig(config.RetentionConfig{GraphQuarantineDays: days}, ToolAuditFloorDays, ""))
		if got.GraphQuarantineDays != days {
			t.Errorf("graph_quarantine_days %d resolved to %d", days, got.GraphQuarantineDays)
		}
	}
}
