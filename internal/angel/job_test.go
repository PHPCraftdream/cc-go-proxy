package angel

import "testing"

func TestJobText(t *testing.T) {
	tests := []struct {
		name string
		j    jobInfo
		want string
	}{
		{"nothing known", jobInfo{}, "a job without limits"},
		{"wnice -p idle", jobInfo{Flags: jobKillOnClose | jobPriorityClass, Priority: 0x40}, "its processes are killed when the job closes, priority idle"},
		{"cpu cap", jobInfo{Flags: jobKillOnClose, CPUFlags: 1 | 4, CPURate: 5000}, "its processes are killed when the job closes, cpu cap 50%"},
		{"weight based cpu is not a cap", jobInfo{CPUFlags: 1 | 2, CPURate: 5000}, "a job without limits"},
		{"memory", jobInfo{Flags: jobJobMemory | jobProcessMemory, JobMem: 4 << 30, ProcessMem: 1 << 30}, "memory cap 4096 MiB, memory cap 1024 MiB per process"},
		{"processes and breakaway", jobInfo{Flags: jobActiveProcess | jobBreakawayOK, ActiveProcesses: 12}, "at most 12 processes, breakaway allowed"},
		{"odd priority", jobInfo{Flags: jobPriorityClass, Priority: 0x1234}, "priority class 0x1234"},
	}
	for _, tt := range tests {
		if got := jobText(tt.j); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Whatever job the test runner is in, asking about it works.
func TestJobNoteAnswers(t *testing.T) {
	if jobNote() == "" {
		t.Error("jobNote is empty")
	}
}
