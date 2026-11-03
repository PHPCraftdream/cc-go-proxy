//go:build windows

package angel

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobNote describes the job object this process lives in ("none" outside of
// one). A job that kills its processes when it closes (wnice, capc) takes the
// server along with the terminal or program that owns it.
func jobNote() string {
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// A nil job handle asks about the job of the calling process; outside of a
	// job the call fails.
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil); err != nil {
		return "none"
	}
	var cpu struct{ Flags, Rate uint32 } // JOBOBJECT_CPU_RATE_CONTROL_INFORMATION
	_ = windows.QueryInformationJobObject(0, windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu)), nil)
	b := ext.BasicLimitInformation
	return jobText(jobInfo{
		Flags: b.LimitFlags, Priority: b.PriorityClass,
		ProcessMem: uint64(ext.ProcessMemoryLimit), JobMem: uint64(ext.JobMemoryLimit),
		ActiveProcesses: b.ActiveProcessLimit, CPUFlags: cpu.Flags, CPURate: cpu.Rate,
	})
}
