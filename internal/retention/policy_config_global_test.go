package retention

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #394: the derived-copy horizons and the upgrade-dump policy are built
// from config by one builder shared by the daemon sweeper, `vornikctl
// retention upgrade-dumps` and the doctor check (design §8.2.4), as §7 did for
// Policy. Every int field present by name on both RetentionConfig and
// GlobalPolicy must be copied; the dump policy's three keys are checked by
// name because they are spelled differently on the two sides.
func TestGlobalFromConfig_CopiesEveryKey(t *testing.T) {
	sv := reflect.New(reflect.TypeOf(config.RetentionConfig{})).Elem()
	want := map[string]int{}
	for i := 0; i < sv.NumField(); i++ {
		f := sv.Type().Field(i)
		if _, ok := reflect.TypeOf(GlobalPolicy{}).FieldByName(f.Name); !ok || f.Type.Kind() != reflect.Int {
			continue
		}
		sv.Field(i).SetInt(int64(2000 + i))
		want[f.Name] = 2000 + i
	}
	for _, name := range []string{"BlackboxTraceCacheDays", "ProjectGistsDays", "ResponseCacheDays", "EmbeddingCacheDays", "ExecutionRatingsDays"} {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is not a field of both RetentionConfig and GlobalPolicy", name)
		}
	}
	rc := sv.Interface().(config.RetentionConfig)
	rc.UpgradeDumpKeep, rc.UpgradeDumpGraceDays, rc.UpgradeDumpDir = 3, 21, "/srv/backups"
	g, d := GlobalFromConfig(rc)
	got := reflect.ValueOf(g)
	for name, w := range want {
		if v := int(got.FieldByName(name).Int()); v != w {
			t.Errorf("GlobalPolicy.%s = %d, want %d (not copied)", name, v, w)
		}
	}
	if d.Keep != 3 || d.GraceDays != 21 || d.Root != "/srv/backups" {
		t.Errorf("UpgradeDumpPolicy = %+v, want keep 3, grace 21, root /srv/backups", d)
	}
	t.Logf("examined %d shared GlobalPolicy fields", len(want))
}
