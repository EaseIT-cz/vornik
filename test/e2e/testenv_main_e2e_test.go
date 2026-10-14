//go:build e2e && !e2e_http && !e2e_migrate

package e2e

import (
	"os"
	"testing"

	"vornik.io/vornik/internal/testutil/testenv"
)

// TestMain runs the e2e-tagged tests in the environment CI gives them: no
// inherited VORNIK_*, so a program a test starts without an explicit Env
// still sees none (GitHub #357, testing design 19 §11). The e2e_http and
// e2e_migrate binaries have their own TestMain, which does the same.
func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
