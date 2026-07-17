package importer

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
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

// openRegularAt opens one regular-file child relative to a pinned
// directory descriptor. It is the content counterpart to
// openDirectoryAt: no path component is re-resolved after the audit,
// and a replacement at the digest name cannot redirect or block.
func openRegularAt(parent *os.File, name string, notRegularErr error) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("invalid child file name %q: %w", name, notRegularErr)
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap child file %q descriptor: %w", name, notRegularErr)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("child %q is not a regular file (mode %s): %w", name, fi.Mode().Type(), notRegularErr)
	}
	return f, nil
}

// openDirectory applies the same untrusted-boundary discipline to a
// directory: never follow a final symlink, never block on a FIFO or
// device, ask the kernel for a directory, then confirm the descriptor's
// type. A parent listing is not authority because the entry can be
// replaced before this open.
func openDirectory(name string, notDirectoryErr error) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0) //nolint:gosec // G304: hardened open of a daemon-supplied handoff path; type is verified below
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("%q is not a directory (mode %s): %w", name, fi.Mode().Type(), notDirectoryErr)
	}
	return f, nil
}

// openDirectoryAt opens one child component relative to a pinned parent
// descriptor. The single-component restriction plus O_NOFOLLOW means a
// swapped intermediate path cannot redirect traversal: callers retain
// the exact parent inode they listed and open its child directly.
func openDirectoryAt(parent *os.File, name string, notDirectoryErr error) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("invalid child directory name %q: %w", name, notDirectoryErr)
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap child directory %q descriptor: %w", name, notDirectoryErr)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("child %q is not a directory (mode %s): %w", name, fi.Mode().Type(), notDirectoryErr)
	}
	return f, nil
}
