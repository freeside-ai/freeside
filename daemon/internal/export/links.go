package export

import (
	"fmt"
	"io/fs"
)

// checkLinks enforces Options.RefuseLinkedFiles on one regular file's lstat.
// A link count the platform can't report refuses too, so the option never
// passes a file it couldn't check.
func (o Options) checkLinks(p string, info fs.FileInfo) error {
	if !o.RefuseLinkedFiles {
		return nil
	}
	n, ok := linkCount(info)
	if !ok {
		return fmt.Errorf("file %q: link count unavailable: %w", p, ErrLinkedFile)
	}
	if n > 1 {
		return fmt.Errorf("file %q has %d links: %w", p, n, ErrLinkedFile)
	}
	return nil
}
