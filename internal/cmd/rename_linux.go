package cmd

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath, failing with an error that wraps
// fs.ErrExist if newpath exists. A filesystem without RENAME_NOREPLACE answers
// EINVAL, and a kernel without renameat2 ENOSYS; both are reported as
// errors.ErrUnsupported, which is what sends publishFile to its fallback.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", errors.ErrUnsupported, err)
	}
	return err
}
