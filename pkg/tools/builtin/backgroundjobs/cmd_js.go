//go:build js

package backgroundjobs

import (
	"errors"
	"os"
	"syscall"
)

type processGroup struct{}

func platformSpecificSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func createProcessGroup(_ *os.Process) (*processGroup, error) {
	return &processGroup{}, nil
}

func terminateProcess(_ *os.Process, _ *processGroup, _ bool) error {
	return errors.New("background_jobs: process termination not supported on js/wasm")
}
