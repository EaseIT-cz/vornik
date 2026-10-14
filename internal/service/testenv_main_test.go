package service

import (
	"os"
	"testing"

	"vornik.io/vornik/internal/testutil/testenv"
)

// TestMain runs this package's tests in the environment CI gives them: these
// tests link the daemon's container, which writes under $VORNIK_DATA_DIR, and
// a developer shell can export production's (GitHub #357, testing design 19
// section 11; enforced by internal/architecture's test-environment law L1).
func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
