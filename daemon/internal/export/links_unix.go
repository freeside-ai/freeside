//go:build unix

package export

import (
	"io/fs"
	"syscall"
)

// linkCount reads a file's hard-link count from the lstat behind info. A
// FileInfo that didn't come from the operating system (an in-memory fs.FS)
// has none.
func linkCount(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true //nolint:unconvert // Nlink is uint16 on darwin, uint64 on linux
}
