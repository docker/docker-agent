package tui_test

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/testutil"
)

func TestMain(m *testing.M) {
	// The TUI exercises real state paths under a per-test HOME.
	os.Exit(testutil.RunWithTempHome(m.Run))
}
