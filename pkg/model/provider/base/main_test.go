package base

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithTempHome(m.Run))
}

func TestTempHomeIsolation(t *testing.T) {
	testutil.RequireTempHome(t)
}
