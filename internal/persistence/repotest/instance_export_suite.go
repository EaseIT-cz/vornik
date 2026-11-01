package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunInstanceExportSuite pins the owner export registry on BOTH backends
// (GitHub #581 D5): a registered id reads back its creation time, an
// unregistered one reads as absent, and a second registration is refused.
func RunInstanceExportSuite(t *testing.T, repo persistence.InstanceExportRepository) {
	ctx := context.Background()

	t.Run("a registered id reads back its creation time", func(t *testing.T) {
		id := uniqueID("x-")
		at := time.Now().UTC().Truncate(time.Millisecond)
		wantOK(t, "RegisterExport", repo.RegisterExport(ctx, id, at))
		got, ok, err := repo.ExportCreatedAt(ctx, id)
		wantOK(t, "ExportCreatedAt", err)
		if !ok || !got.Equal(at) {
			t.Fatalf("ExportCreatedAt = %v, %v; want %v, true", got, ok, at)
		}
	})

	t.Run("an id with no record reads as absent", func(t *testing.T) {
		_, ok, err := repo.ExportCreatedAt(ctx, uniqueID("x-"))
		wantOK(t, "ExportCreatedAt", err)
		if ok {
			t.Fatal("an unregistered id read as registered")
		}
	})

	t.Run("a second registration of one id is refused and keeps the first time", func(t *testing.T) {
		id := uniqueID("x-")
		first := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		wantOK(t, "RegisterExport", repo.RegisterExport(ctx, id, first))
		if err := repo.RegisterExport(ctx, id, time.Now().UTC()); !errors.Is(err, persistence.ErrExportExists) {
			t.Fatalf("second RegisterExport = %v; want ErrExportExists", err)
		}
		got, _, err := repo.ExportCreatedAt(ctx, id)
		wantOK(t, "ExportCreatedAt", err)
		if !got.Equal(first) {
			t.Fatalf("the creation time moved to %v; want %v (a re-registration must not extend the 7 days)", got, first)
		}
	})
}
