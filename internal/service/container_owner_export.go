package service

import (
	"context"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/artifacts/s3"
	"vornik.io/vornik/internal/config"
)

// ownerExportOption wires the owner export (GitHub #581 D5,
// enterprise-packaging-design.md amendment 2026-10-29, the D5 correction):
// on a managed instance with hosting.backup_bucket set and a registry, the
// backup bucket's S3 client with no static keys, so the SDK's default chain
// (the instance role) reads it, and S3 decrypts it (SSE-KMS). Otherwise
// nothing is wired, and the endpoints answer 503.
func (c *Container) ownerExportOption() api.ServerOption {
	store, err := c.ownerExportStore()
	if err != nil {
		c.Logger.Warn().Err(err).Msg("owner export: the backup bucket's client could not be built; exports answer 503")
	}
	if store == nil || c.repos == nil || c.repos.InstanceExports == nil {
		return api.WithOwnerExports(nil, nil)
	}
	return api.WithOwnerExports(c.repos.InstanceExports, store)
}

// ownerExportStore builds the backup bucket's client, or nil when exports
// are not configured.
func (c *Container) ownerExportStore() (api.ExportObjectStore, error) {
	if c.Config == nil || !c.Config.Hosting.Managed() || c.Config.Hosting.BackupBucket == "" {
		return nil, nil
	}
	return s3.New(context.Background(), ownerExportS3Options(c.Config))
}

// ownerExportS3Options are the backup bucket's options: no prefix (the key
// carries instances/<id>/), no endpoint and no keys, so the instance role is
// used.
func ownerExportS3Options(cfg *config.Config) s3.Options {
	region := cfg.Hosting.BackupRegion
	if region == "" {
		region = cfg.Storage.S3.Region
	}
	return s3.Options{Bucket: cfg.Hosting.BackupBucket, Region: region, ForceSSL: true}
}
