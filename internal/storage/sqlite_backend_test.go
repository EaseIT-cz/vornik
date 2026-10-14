package storage

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// #367 (2026-10-14): internal/service unit-test containers take a copy of the
// migrated in-memory image instead of storage.Open migrating from empty, and
// wrap it with NewSQLiteBackend. That is only the same container if
// NewSQLiteBackend is what openSQLite builds after connect + migrate: the same
// driver, the same repositories, and Close/Migrate/IsReady bound to the
// database (storage-abstraction design, "In-memory test databases").
func TestNewSQLiteBackend_IsWhatOpenBuildsAfterMigrate(t *testing.T) {
	opened, err := Open(sqlitetest.Context(t), config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	db := sqlitetest.Memory(t)
	wrapped := NewSQLiteBackend(db)

	if wrapped.Driver != opened.Driver || wrapped.PG != nil {
		t.Fatalf("driver %q (PG %v), want %q and no Postgres handle", wrapped.Driver, wrapped.PG, opened.Driver)
	}
	if wrapped.DB != db.DB {
		t.Fatal("the backend's DB is not the database it was given")
	}
	if wrapped.Close == nil || wrapped.Migrate == nil || wrapped.IsReady == nil {
		t.Fatal("Close, Migrate and IsReady must all be bound")
	}
	ow, ov := reflect.ValueOf(*wrapped.Repos), reflect.ValueOf(*opened.Repos)
	checked := 0
	for i := 0; i < ov.NumField(); i++ {
		name := ov.Type().Field(i).Name
		if ov.Field(i).IsNil() != ow.Field(i).IsNil() {
			t.Errorf("repository %s: nil after Open=%v, nil from NewSQLiteBackend=%v", name, ov.Field(i).IsNil(), ow.Field(i).IsNil())
		}
		if ow.Field(i).Kind() == reflect.Interface && !ow.Field(i).IsNil() && ow.Field(i).Elem().Type() != ov.Field(i).Elem().Type() {
			t.Errorf("repository %s: %s from NewSQLiteBackend, %s from Open", name, ow.Field(i).Elem().Type(), ov.Field(i).Elem().Type())
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("compared %d repository fields; the walk reached too few to prove anything", checked)
	}
	t.Logf("compared %d repository fields", checked)
	if err := wrapped.IsReady(sqlitetest.Context(t)); err != nil {
		t.Fatalf("IsReady on the wrapped migrated copy: %v", err)
	}
	if err := wrapped.Migrate(sqlitetest.Context(t)); err != nil {
		t.Fatalf("Migrate on the wrapped migrated copy: %v", err)
	}
}
