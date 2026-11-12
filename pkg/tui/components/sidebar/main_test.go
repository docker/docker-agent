package sidebar

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

// The test binary runs inside the repository checkout; pin the working
// directory and branch so layouts do not depend on the checkout's branch name.
func TestMain(m *testing.M) {
	restore := SetWorkingDirectoryForTesting("/work/app", "")
	code := testutil.RunWithTempHome(m.Run)
	restore()
	os.Exit(code)
}
