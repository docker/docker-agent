//go:build unix

package worktree

import "golang.org/x/sys/unix"

const nonblockFlag = unix.O_NONBLOCK
