package ocisource

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// Tests re-home the default content store via HOME; clear XDG variables
	// so artifacts never land in the developer's real data directory.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
