//go:build !windows

package angel

// jobNote: job objects are a Windows thing.
func jobNote() string { return "none" }
