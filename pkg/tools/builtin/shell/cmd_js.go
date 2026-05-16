//go:build js

package shell

import (
	"errors"
	"os"
	"syscall"
)

// processGroup is a no-op under js/wasm: there are no processes to track,
// because os/exec cannot actually spawn subprocesses in the browser.
type processGroup struct{}

func platformSpecificSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func createProcessGroup(_ *os.Process) (*processGroup, error) {
	return &processGroup{}, nil
}

func kill(_ *os.Process, _ *processGroup) error {
	return errors.New("shell: process termination not supported on js/wasm")
}
