//go:build windows

package angel

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job object limit flags and priority classes (winnt.h).
const (
	jobAffinity        = 0x10
	jobPriorityClass   = 0x20
	jobActiveProcess   = 0x8
	jobProcessMemory   = 0x100
	jobJobMemory       = 0x200
	jobBreakawayOK     = 0x800
	jobSilentBreakaway = 0x1000
	jobKillOnClose     = 0x2000
)

// jobInfo is what the job object around a process limits.
type jobInfo struct {
	Flags           uint32
	Priority        uint32
	ProcessMem      uint64
	JobMem          uint64
	ActiveProcesses uint32
	CPUFlags        uint32
	CPURate         uint32 // of the hard cap, in 1/100 percent
}

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

// jobText says in a few words what a job object does to its processes: a server
// that lives in one dies with it, and runs slower under its caps.
func jobText(j jobInfo) string {
	var p []string
	if j.Flags&jobKillOnClose != 0 {
		p = append(p, "its processes are killed when the job closes")
	}
	if j.Flags&jobPriorityClass != 0 {
		p = append(p, "priority "+priorityName(j.Priority))
	}
	if j.CPUFlags&1 != 0 && j.CPUFlags&4 != 0 && j.CPURate > 0 {
		p = append(p, fmt.Sprintf("cpu cap %d%%", j.CPURate/100))
	}
	if j.Flags&jobJobMemory != 0 {
		p = append(p, fmt.Sprintf("memory cap %d MiB", j.JobMem>>20))
	}
	if j.Flags&jobProcessMemory != 0 {
		p = append(p, fmt.Sprintf("memory cap %d MiB per process", j.ProcessMem>>20))
	}
	if j.Flags&jobActiveProcess != 0 {
		p = append(p, fmt.Sprintf("at most %d processes", j.ActiveProcesses))
	}
	if j.Flags&jobAffinity != 0 {
		p = append(p, "cpu affinity")
	}
	if j.Flags&(jobBreakawayOK|jobSilentBreakaway) != 0 {
		p = append(p, "breakaway allowed")
	}
	if len(p) == 0 {
		return "a job without limits"
	}
	return strings.Join(p, ", ")
}

func priorityName(class uint32) string {
	switch class {
	case 0x40:
		return "idle"
	case 0x4000:
		return "below normal"
	case 0x20:
		return "normal"
	case 0x8000:
		return "above normal"
	case 0x80:
		return "high"
	case 0x100:
		return "realtime"
	}
	return fmt.Sprintf("class %#x", class)
}
