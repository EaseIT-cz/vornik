package persistence

import (
	"context"
	"errors"
	"time"
)

// ErrExportExists is RegisterExport's answer for an id already registered.
var ErrExportExists = errors.New("export id already registered")

// InstanceExportRepository is the registry of owner exports (GitHub #581 D5,
// enterprise-packaging-design.md amendment 2026-10-29). The control plane
// registers an export it has written to the instance's backup prefix; the
// daemon serves it to the owner only while its own record of the creation is
// younger than 7 days, and never an id it holds no record of.
type InstanceExportRepository interface {
	// RegisterExport records id as created at createdAt. A second
	// registration of one id is ErrExportExists: an id names one object.
	RegisterExport(ctx context.Context, id string, createdAt time.Time) error
	// ExportCreatedAt returns when id was registered, and false when it was
	// not.
	ExportCreatedAt(ctx context.Context, id string) (time.Time, bool, error)
}
