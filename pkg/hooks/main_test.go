package hooks

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	warmShell()
	os.Exit(m.Run())
}
