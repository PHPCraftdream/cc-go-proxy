//go:build !windows

package claudehome

import "os"

func isRedirect(_ string, info os.FileInfo) (bool, error) {
	return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0, nil
}
