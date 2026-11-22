package builtins

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// Snapshot tests write under the data dir; keep them out of the real one.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
