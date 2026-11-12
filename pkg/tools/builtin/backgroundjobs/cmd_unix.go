//go:build !windows && !js

package backgroundjobs

import (
	"os"
	"syscall"
)

type processGroup struct{}

func platformSpecificSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func createProcessGroup(_ *os.Process) (*processGroup, error) {
	return &processGroup{}, nil
}

func terminateProcess(proc *os.Process, _ *processGroup, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-proc.Pid, signal)
}
