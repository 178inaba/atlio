package cmd

import "golang.org/x/sys/unix"

// renameNoReplace renames oldpath to newpath, failing with an error that wraps
// fs.ErrExist if newpath exists. A volume without RENAME_EXCL answers ENOTSUP,
// which matches errors.ErrUnsupported and sends publishFile to its fallback.
func renameNoReplace(oldpath, newpath string) error {
	return unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL)
}
