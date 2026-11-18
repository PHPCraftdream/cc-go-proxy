//go:build !windows

package safefs

import "os"

// IsRedirect reports whether a path is a link, junction or other redirection.
func IsRedirect(_ string, info os.FileInfo) (bool, error) {
	return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0, nil
}
