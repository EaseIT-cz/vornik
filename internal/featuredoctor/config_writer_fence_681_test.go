package featuredoctor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #681: LockConfigFile, which ApplyEnable (feature enable and the
// fix-it adapter) and integrations.Save take, refuses on a daemon that does
// not hold the config_writer lease.
func TestLockConfigFile681_RefusedOnANonWriterDaemon(t *testing.T) {
	configlock.SetWriterCheck(func(context.Context) (bool, string) { return false, "lease held by node-b" })
	t.Cleanup(func() { configlock.SetWriterCheck(nil) })
	if _, err := LockConfigFile(context.Background(), "/etc/vornik/config.yaml"); !errors.Is(err, configlock.ErrNotConfigWriter) {
		t.Fatalf("err = %v, want ErrNotConfigWriter", err)
	}
}
