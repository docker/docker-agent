//go:build !windows

package acp

func terminalInterpreter() (string, []string) {
	return "/bin/sh", []string{"-c"}
}
