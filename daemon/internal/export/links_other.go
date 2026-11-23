//go:build !unix

package export

import "io/fs"

// linkCount has no source off unix, so RefuseLinkedFiles refuses every
// regular file there.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
