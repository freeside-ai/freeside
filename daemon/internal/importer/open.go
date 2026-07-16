package importer

import (
	"fmt"
	"os"
	"syscall"
)

// openRegular opens a file on the untrusted handoff boundary safely: it
// refuses to follow a final symlink (O_NOFOLLOW, so a symlink planted at
// the path cannot redirect the read outside the handoff) and never
// blocks on a FIFO or device open (O_NONBLOCK), then confirms the opened
// descriptor is a regular file and fails closed otherwise. A hostile
// handoff can hold any inode type at any name, so every open here is
// guarded rather than trusting a prior directory listing (which a swap
// could invalidate). The reference deployment is Linux/macOS (plan
// §3.3), where both flags exist.
func openRegular(name string, notRegularErr error) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: hardened open of a daemon-supplied handoff path; type is verified below
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%q is not a regular file (mode %s): %w", name, fi.Mode().Type(), notRegularErr)
	}
	return f, nil
}
