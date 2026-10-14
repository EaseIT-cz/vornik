package hermes

import (
	"os"
	"testing"

	"vornik.io/vornik/internal/testutil/testenv"
)

// TestMain runs the lane, and this package's untagged tests, in the
// environment CI gives them: no inherited VORNIK_* except the VORNIK_E2E_*
// knobs the lane reads itself (GitHub #357, testing design 19 §11).
func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
