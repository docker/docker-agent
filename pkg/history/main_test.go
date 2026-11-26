package history

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// History defaults to the data dir under HOME; clear XDG variables so
	// tests isolating via HOME do not share the developer's state.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
