package shell

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processGroup struct {
	jobHandle     windows.Handle
	processHandle windows.Handle
}

func platformSpecificSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func createProcessGroup(proc *os.Process) (*processGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		_ = windows.CloseHandle(handle)
		_ = windows.CloseHandle(job)
		return nil, err
	}

	return &processGroup{
		jobHandle:     job,
		processHandle: handle,
	}, nil
}

func kill(proc *os.Process, pg *processGroup) error {
	if pg != nil {
		// Close handles to trigger JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		// which will terminate all processes in the job
		if pg.processHandle != 0 {
			_ = windows.CloseHandle(pg.processHandle)
		}
		if pg.jobHandle != 0 {
			_ = windows.CloseHandle(pg.jobHandle)
		}
	}

	// Also call Kill on the process as a fallback
	return proc.Kill()
}
