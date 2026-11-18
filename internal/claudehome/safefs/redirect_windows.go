package safefs

import (
	"os"

	"golang.org/x/sys/windows"
)

// IsRedirect reports whether a path is a link, junction or other redirection.
func IsRedirect(path string, info os.FileInfo) (bool, error) {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true, nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
