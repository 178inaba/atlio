//go:build !linux && !darwin && !windows

package cmd

import "errors"

// renameNoReplace has no no-replace rename to call on this platform, so every
// download takes publishFile's O_EXCL fallback.
func renameNoReplace(string, string) error {
	return errors.ErrUnsupported
}
