package service

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #581 D5: the backup bucket's client takes no static keys and no
// endpoint (the instance role on AWS), the hosting region before the
// storage one, and is built on a managed instance with a bucket only.
func TestOwnerExport581_TheBackupBucketsOptions(t *testing.T) {
	cfg := &config.Config{}
	cfg.Hosting.BackupBucket = "vornik-backups"
	cfg.Storage.S3.Region = "eu-west-1"
	cfg.Storage.S3.AccessKeyID, cfg.Storage.S3.SecretAccessKey = "AKIA-artifacts", "artifact-secret"
	o := ownerExportS3Options(cfg)
	if o.Bucket != "vornik-backups" || o.Region != "eu-west-1" {
		t.Errorf("options %+v; want the bucket and storage.s3.region", o)
	}
	if o.AccessKeyID != "" || o.SecretAccessKey != "" || o.Endpoint != "" || o.Prefix != "" {
		t.Errorf("options %+v carry the artifact store's keys, endpoint or a prefix", o)
	}
	cfg.Hosting.BackupRegion = "eu-central-1"
	if got := ownerExportS3Options(cfg).Region; got != "eu-central-1" {
		t.Errorf("region %q; want hosting.backup_region first", got)
	}

	for name, c := range map[string]*Container{
		"no config":                 {},
		"a self host with a bucket": {Config: cfg},
		"managed without a bucket":  {Config: func() *config.Config { m := &config.Config{}; m.Hosting.Mode = config.HostingModeManaged; return m }()},
	} {
		if store, err := c.ownerExportStore(); store != nil || err != nil {
			t.Errorf("%s: store %v, err %v; want nothing built", name, store, err)
		}
	}
}
