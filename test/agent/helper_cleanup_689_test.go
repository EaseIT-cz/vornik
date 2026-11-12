package agent_test

import (
	"os"
	"testing"
)

// GitHub #689: buildHelper's directory was never removed. A test cannot see
// its own binary's TestMain finish, so this pins the two halves: buildHelper
// registers the directory it created, and removeHelperTemp (what TestMain
// calls after m.Run) removes the registered directory.
func TestBuildHelper_RegistersItsDirForRemoval_689(t *testing.T) {
	dir := buildHelper(t)
	if helperTemp != dir {
		t.Fatalf("helperTemp = %q, want the build dir %q", helperTemp, dir)
	}
}

func TestRemoveHelperTemp_RemovesTheRegisteredDir_689(t *testing.T) {
	saved := helperTemp
	t.Cleanup(func() { helperTemp = saved })
	dir, err := os.MkdirTemp("", "vornik-agent-helper-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/vornik-agent-helper", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	helperTemp = dir
	removeHelperTemp()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		_ = os.RemoveAll(dir)
		t.Fatalf("the registered dir survived removeHelperTemp: %v", err)
	}
}
