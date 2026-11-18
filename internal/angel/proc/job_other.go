//go:build !windows

package proc

// JobNote describes the Windows job object the process lives in; job objects are a Windows thing.
func JobNote() string { return "none" }
