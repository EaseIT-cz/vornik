package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #584 (operations-api-contract-design.md §6.6): one process-wide
// config-write lock, internal/configlock. Two separate mutexes (api's and the
// control plane's) and a third, path-keyed one (featuredoctor's) each guarded
// some writers, and several writers held none. This law keeps it one: every
// non-test site that constructs a featuredoctor.FileConfigWriter is a reviewed
// census entry naming what holds the lock, and no package declares a
// config-write mutex of its own.
var configWriterSites = map[string]string{
	"internal/api/credentials_rotate_582.go":       "RotateOperationsCredential holds configlock",
	"internal/api/setup_managed_582.go":            "SetupManaged holds configlock in both phases (the probe runs between them, unlocked)",
	"internal/api/setup_handlers.go":               "writeChatConfig and writeDispatcherProjectConfig hold configlock",
	"internal/api/setup_memory_handlers.go":        "writeMemoryConfig holds configlock",
	"internal/api/featuredoctor_enable_handler.go": "passes the writer to featuredoctor.ApplyEnable, which holds LockConfigFile (configlock)",
	"internal/service/fixit_dispatch_adapter.go":   "passes the writer to featuredoctor.ApplyEnable, which holds LockConfigFile (configlock)",
	"internal/integrations/save.go":                "Save holds LockConfigFile (configlock) before it builds the writer",
}

var (
	fileConfigWriterRe = regexp.MustCompile(`FileConfigWriter\{`)
	// A config-write mutex of a package's own: the shape the old locks had.
	ownConfigMutexRe = regexp.MustCompile(`(?m)^\s*(var\s+)?(configWriteMu|configFileLocks)\s+(sync\.Mutex|sync\.Map)`)
)

func TestConfigLock584_EveryConfigWriterIsReviewed(t *testing.T) {
	root := moduleRoot(t)
	got := map[string]int{}
	var ownMutex []string
	examined := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name(), "testdata", "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "cmd/") {
			// The offline commands (provision, genconfig, migrate-ce) run
			// without a daemon, so a process lock does not apply.
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		examined++
		if n := len(fileConfigWriterRe.FindAll(raw, -1)); n > 0 {
			got[rel] = n
		}
		if ownConfigMutexRe.Match(raw) {
			ownMutex = append(ownMutex, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var unreviewed, stale []string
	for f := range got {
		if _, ok := configWriterSites[f]; !ok {
			unreviewed = append(unreviewed, f)
		}
	}
	for f := range configWriterSites {
		if got[f] == 0 {
			stale = append(stale, f)
		}
	}
	sort.Strings(unreviewed)
	sort.Strings(stale)
	if len(unreviewed) > 0 {
		t.Errorf("FileConfigWriter constructed in files outside the census: %v. Hold configlock across the read, write and validate, and list the file in configWriterSites with what holds it", unreviewed)
	}
	if len(stale) > 0 {
		t.Errorf("census entries with no FileConfigWriter site any more: %v", stale)
	}
	if len(ownMutex) > 0 {
		t.Errorf("a package-level config-write mutex of its own in %v: use internal/configlock, the process's one lock", ownMutex)
	}
	t.Logf("config writer census: %d non-test Go files examined, %d with a FileConfigWriter site", examined, len(got))
}
