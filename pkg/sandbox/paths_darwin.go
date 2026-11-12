//go:build darwin && cgo

package sandbox

/*
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/param.h>
#include <unistd.h>

static int stored_path(const char *path, char *out) {
	int fd = open(path, O_EVTONLY | O_SYMLINK | O_CLOEXEC);
	if (fd < 0) return errno;
	int result;
	do { result = fcntl(fd, F_GETPATH, out); } while (result < 0 && errno == EINTR);
	int error = result < 0 ? errno : 0;
	close(fd);
	return error;
}
*/
import "C"

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func storedPathCase(path string) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", &os.PathError{Op: "fcntl", Path: path, Err: syscall.EINVAL}
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var buf [C.MAXPATHLEN]C.char
	if errno := C.stored_path(cpath, &buf[0]); errno != 0 {
		if syscall.Errno(errno) == syscall.ENOENT {
			return path, nil
		}
		return "", &os.PathError{Op: "fcntl", Path: path, Err: syscall.Errno(errno)}
	}
	return C.GoString(&buf[0]), nil
}
