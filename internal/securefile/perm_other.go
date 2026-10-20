//go:build !windows

package securefile

import "os"

func RestrictToOwner(path string) error {
	return os.Chmod(path, 0o600)
}
