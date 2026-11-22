package remote

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// Pull tests construct the default content store under a temporary HOME.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
