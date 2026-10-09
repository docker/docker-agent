//go:build darwin || linux

package desktop

import "syscall"

// errConnRefused is returned when dialing a socket nothing listens on.
const errConnRefused = syscall.ECONNREFUSED
