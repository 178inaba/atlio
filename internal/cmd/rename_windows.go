package cmd

import "golang.org/x/sys/windows"

// renameNoReplace renames oldpath to newpath, failing with an error that wraps
// fs.ErrExist if newpath exists: without MOVEFILE_REPLACE_EXISTING, MoveFileEx
// refuses an existing target. Without MOVEFILE_COPY_ALLOWED it never falls back
// to a copy, which a rename within one directory never needs.
func renameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, 0)
}
