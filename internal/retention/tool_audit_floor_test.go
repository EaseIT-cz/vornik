package retention

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: tool_audit_log had no six-month floor (EU AI Act Art 26(6),
// GitHub #117). Any window from 1 to 179 days — and the compiled default of 30 —
// was accepted silently. Design:
// https://docs.vornik.io §2.

func TestResolveToolAuditFloor_Table(t *testing.T) {
	const basis = "national law X §1"
	cases := []struct {
		name     string
		days     int
		override bool
		basis    string
		want     int
		warns    bool
	}{
		{"unset → six months", 0, false, "", ToolAuditFloorDays, false},
		{"raised above six months", 365, false, "", 365, false},
		{"exactly six months", ToolAuditFloorDays, true, "", ToolAuditFloorDays, false},
		{"lower without override is ignored", 30, false, "", ToolAuditFloorDays, true},
		{"negative without override is ignored", -5, false, "", ToolAuditFloorDays, true},
		{"override without basis is refused", 30, true, "  ", ToolAuditFloorDays, true},
		{"lower with override and basis takes effect", 30, true, basis, 30, true},
		{"override with no value → minimum floor", 0, true, basis, MinimumFloorDays, true},
		{"override with negative value → minimum floor", -3, true, basis, MinimumFloorDays, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveToolAuditFloor(tc.days, tc.override, tc.basis)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.warns, warning != "", "warning=%q", warning)
		})
	}
}

func TestResolve_ToolAuditClampedUpToSixMonthFloor(t *testing.T) {
	// Daemon-level 90 (the production setting on 2026-10-12) → 180.
	got := Resolve("p", Policy{}, Policy{ToolAuditDays: 90})
	assert.Equal(t, ToolAuditFloorDays, got.ToolAuditDays)

	// A project cannot go below the floor either.
	got = Resolve("p", Policy{ToolAuditDays: 30}, Policy{ToolAuditDays: 400})
	assert.Equal(t, ToolAuditFloorDays, got.ToolAuditDays)

	// A window above the floor is untouched.
	got = Resolve("p", Policy{ToolAuditDays: 200}, Policy{})
	assert.Equal(t, 200, got.ToolAuditDays)

	// A raised floor lifts a window that clears six months but not the floor.
	got = Resolve("p", Policy{ToolAuditDays: 200}, Policy{ToolAuditFloorDays: 365})
	assert.Equal(t, 365, got.ToolAuditDays)

	// A project-level floor is ignored: the floor is deployer posture.
	got = Resolve("p", Policy{ToolAuditDays: 30, ToolAuditFloorDays: 7}, Policy{})
	assert.Equal(t, ToolAuditFloorDays, got.ToolAuditDays)
}

func TestResolve_ToolAuditFloorOverrideHonoured(t *testing.T) {
	floor, _ := ResolveToolAuditFloor(30, true, "national law X")
	got := Resolve("p", Policy{ToolAuditDays: 30}, Policy{ToolAuditFloorDays: floor})
	assert.Equal(t, 30, got.ToolAuditDays)
	assert.Equal(t, 30, got.ToolAuditFloorDays)
}

func TestResolve_CompiledToolAuditDefaultIsNotBelowFloor(t *testing.T) {
	assert.GreaterOrEqual(t, DefaultToolAuditDays, ToolAuditFloorDays)
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, ToolAuditFloorDays, got.ToolAuditDays)
}

// A Policy built WITHOUT Resolve (the admin-UI preview did exactly that) must
// still not prune under the floor: the sweeper re-applies the clamp.
func TestSweep_ToolAuditFloorEnforcedWithoutResolve(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var gotThreshold time.Time
	mock.ExpectQuery("SELECT COUNT.*FROM task_llm_usage").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT.*FROM tool_audit_log").
		WithArgs(thresholdCapture{&gotThreshold}, "p").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	// Later steps hit no expectation and fail; Preview logs and carries on.

	s := New(db, zerolog.Nop())
	_, _ = s.Preview(context.Background(), Policy{ProjectID: "p", ToolAuditDays: 30})

	require.False(t, gotThreshold.IsZero(), "tool_audit_log query never issued")
	age := time.Since(gotThreshold)
	assert.GreaterOrEqual(t, age, time.Duration(ToolAuditFloorDays)*24*time.Hour-time.Minute,
		"threshold %s is younger than the six-month floor", age)
}

// A real (in-memory) database, not a threshold assertion: the rows that
// survive an actual Sweep are what the floor promises (review F5). Ages straddle
// the 180-day edge; the predicate is created_at < now − window, so a row one
// day inside the window survives and one day outside is pruned.
func TestSweep_ToolAuditFloor_RealRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE tool_audit_log (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`)
	require.NoError(t, err)
	seed := func() {
		_, err := db.Exec(`DELETE FROM tool_audit_log`)
		require.NoError(t, err)
		for id, days := range map[string]int{"d029": 29, "d100": 100, "d179": 179, "d181": 181, "d400": 400} {
			_, err := db.Exec(`INSERT INTO tool_audit_log (id, project_id, created_at) VALUES (?, 'p', ?)`,
				id, persistence.SQLiteTime(time.Now().UTC().AddDate(0, 0, -days))) // the stored form (#360)
			require.NoError(t, err)
		}
	}
	ids := func() []string {
		rows, err := db.Query(`SELECT id FROM tool_audit_log ORDER BY id`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			out = append(out, id)
		}
		require.NoError(t, rows.Err())
		return out
	}
	s := New(db, zerolog.Nop())

	// A hand-built policy asking for 30 days: the floor holds anyway. (Other
	// steps fail on tables this DB lacks; Sweep logs them and carries on.)
	seed()
	_, _ = s.Sweep(context.Background(), Policy{ProjectID: "p", ToolAuditDays: 30})
	assert.Equal(t, []string{"d029", "d100", "d179"}, ids())

	// Through Resolve with the daemon at production's 90: same outcome.
	seed()
	_, _ = s.Sweep(context.Background(), Resolve("p", Policy{}, Policy{ToolAuditDays: 90}))
	assert.Equal(t, []string{"d029", "d100", "d179"}, ids())

	// The override with a basis lets 30 days take effect.
	floor, _ := ResolveToolAuditFloor(30, true, "national law X")
	seed()
	_, _ = s.Sweep(context.Background(), Resolve("p", Policy{ToolAuditDays: 30}, Policy{ToolAuditFloorDays: floor}))
	assert.Equal(t, []string{"d029"}, ids())
}

// Resolve and the sweeper must agree on the window for every input, because
// both go through Policy.toolAuditWindow (review F6).
func TestToolAuditWindow_ResolveAndSweeperAgree(t *testing.T) {
	for _, floorCfg := range []struct {
		days     int
		override bool
	}{{0, false}, {30, true}, {365, false}} {
		floor, _ := ResolveToolAuditFloor(floorCfg.days, floorCfg.override, "basis")
		for _, days := range []int{-1, 0, 1, 30, 90, 179, 180, 181, 400} {
			resolved := Resolve("p", Policy{ToolAuditDays: days}, Policy{ToolAuditFloorDays: floor})
			raw := Policy{ToolAuditDays: days, ToolAuditFloorDays: floor}
			want := raw.toolAuditWindow()
			if days <= 0 { // Resolve fills an unset window from the compiled default first
				want = Policy{ToolAuditDays: DefaultToolAuditDays, ToolAuditFloorDays: floor}.toolAuditWindow()
			}
			assert.Equal(t, want, resolved.ToolAuditDays, "floor=%d days=%d", floor, days)
			assert.GreaterOrEqual(t, resolved.ToolAuditDays, floor)
		}
	}
}

type thresholdCapture struct{ dst *time.Time }

func (c thresholdCapture) Match(v driver.Value) bool {
	if ts, ok := v.(time.Time); ok {
		*c.dst = ts
		return true
	}
	return false
}
