package desktop

import "syscall"

// errConnRefused is WSAECONNREFUSED; Winsock never returns syscall.ECONNREFUSED.
const errConnRefused = syscall.Errno(10061)
