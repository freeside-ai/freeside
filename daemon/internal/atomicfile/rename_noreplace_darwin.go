//go:build darwin

package atomicfile

import "golang.org/x/sys/unix"

// RenameNoReplace atomically renames oldpath to newpath without replacing an
// existing target.
func RenameNoReplace(oldpath, newpath string) error {
	return unix.RenameatxNp(
		unix.AT_FDCWD, oldpath,
		unix.AT_FDCWD, newpath,
		unix.RENAME_EXCL,
	)
}
