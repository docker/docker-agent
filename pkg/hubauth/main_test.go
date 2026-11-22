package hubauth

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// Tests modify the cache directory (including its permissions); keep
	// that confined to a temporary HOME.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
