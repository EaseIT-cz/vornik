package config

import (
	"strings"
	"testing"
)

// GitHub #581 D5 (enterprise-packaging-design.md, amendment 2026-10-29, the
// D5 correction): the owner export reads the backup bucket, managed only,
// under the instance's own id.
func TestHostingConfig581_BackupBucket(t *testing.T) {
	ok := func(c *Config) {
		c.Hosting.InstanceID = "i581"
		c.Hosting.BackupBucket = "vornik-backups-eu-central-1"
		c.Hosting.BackupRegion = "eu-central-1"
	}
	c := managedConfig()
	ok(c)
	if err := c.Validate(); err != nil {
		t.Fatalf("a managed config with a backup bucket: %v", err)
	}
	c = managedConfig()
	ok(c)
	c.Hosting.BackupRegion, c.Storage.S3.Region = "", "eu-central-1"
	if err := c.Validate(); err != nil {
		t.Fatalf("the region from storage.s3.region: %v", err)
	}
	c = managedConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("a managed config without one (exports not configured): %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"set on a self host", func(c *Config) { *c = *baseValidConfig(); c.Hosting.BackupBucket = "b-ucket" }, "hosting.backup_bucket"},
		{"a region alone on a self host", func(c *Config) { *c = *baseValidConfig(); c.Hosting.BackupRegion = "eu-central-1" }, "hosting.backup_bucket"},
		{"no instance id", func(c *Config) { ok(c); c.Hosting.InstanceID = "" }, "hosting.instance_id"},
		{"a region without a bucket", func(c *Config) { ok(c); c.Hosting.BackupBucket = "" }, "hosting.backup_region"},
		{"a bucket with a path", func(c *Config) { ok(c); c.Hosting.BackupBucket = "b/instances" }, "hosting.backup_bucket"},
		{"an upper-case bucket", func(c *Config) { ok(c); c.Hosting.BackupBucket = "Backups" }, "hosting.backup_bucket"},
		{"a short bucket", func(c *Config) { ok(c); c.Hosting.BackupBucket = "ab" }, "hosting.backup_bucket"},
		{"no region anywhere", func(c *Config) { ok(c); c.Hosting.BackupRegion = ""; c.Storage.S3.Region = "" }, "hosting.backup_region"},
		{"a region with a slash", func(c *Config) { ok(c); c.Hosting.BackupRegion = "eu/central" }, "hosting.backup_region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := managedConfig()
			tc.mut(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v; want an error naming %s", err, tc.want)
			}
		})
	}
}
