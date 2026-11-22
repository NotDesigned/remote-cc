//go:build windows

package procjob

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	once sync.Once
	job  windows.Handle // deliberately never closed: closing it is the kill switch
	err  error
)

func killOnExit() error {
	once.Do(func() {
		// nil security attributes: the handle is not inheritable, so no child
		// can keep the job alive after rcc is gone.
		h, e := windows.CreateJobObject(nil, nil)
		if e != nil {
			err = fmt.Errorf("CreateJobObject: %w", e)
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, e := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); e != nil {
			windows.CloseHandle(h)
			err = fmt.Errorf("SetInformationJobObject: %w", e)
			return
		}
		// Nested jobs (Windows 8+) let this succeed even when the terminal
		// already runs us inside a job.
		if e := windows.AssignProcessToJobObject(h, windows.CurrentProcess()); e != nil {
			windows.CloseHandle(h)
			err = fmt.Errorf("AssignProcessToJobObject: %w", e)
			return
		}
		job = h
	})
	return err
}
