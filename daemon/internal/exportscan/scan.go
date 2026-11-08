package exportscan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// credentialRules is the shared credential rule list
// (internal/credentialpattern), the same list the importer applies.
var credentialRules = credentialpattern.All()

// scanChunkBytes is how much of a file is held in memory at once. Files are
// streamed in overlapping chunks rather than truncated: the export's largest
// member is the agent's own transcript, which is both the most likely to
// exceed any fixed cap and the most likely to echo auth material, so a cap
// that returned "clean" for the rest of a large file would be a silent false
// negative in exactly the worst place.
const scanChunkBytes = 1 << 20

// scanOverlapBytes is carried between chunks so a pattern straddling a chunk
// boundary is still matched. It exceeds the longest pattern this scanner can
// match by a wide margin.
const scanOverlapBytes = 4 << 10

// Scanner implements ward.OutputScanner. The zero value is ready to use.
type Scanner struct{}

// Scan walks dir and refuses with ward.ErrOutputScanRefused when any regular
// file in it carries credential material. Any other error is an operational
// failure, not a policy refusal.
func (Scanner) Scan(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// Only regular files are read. The export is already digest-verified
		// and extracted by the gate, but a scanner that followed a symlink
		// would read outside the directory it was asked to inspect.
		if !entry.Type().IsRegular() {
			return nil
		}
		rule, matched, err := scanFile(ctx, path)
		if err != nil {
			return err
		}
		if matched {
			// The match itself is never reported: naming the pattern locates
			// the leak without copying the secret into a log, an attention
			// item, or an error a client can read.
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = filepath.Base(path)
			}
			return fmt.Errorf("%w: credential material matching %s found in exported %s",
				ward.ErrOutputScanRefused, rule.Name, rel)
		}
		return nil
	})
}

// scanFile streams one file through the rule set, returning the first rule
// that matched and true, or false when none did. The whole file is examined
// regardless of its size; only the working set is bounded.
func scanFile(ctx context.Context, path string) (credentialpattern.Rule, bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: gate-extracted export path handed to this scanner
	if err != nil {
		return credentialpattern.Rule{}, false, fmt.Errorf("scan export file: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 0, scanChunkBytes+scanOverlapBytes)
	chunk := make([]byte, scanChunkBytes)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return credentialpattern.Rule{}, false, ctxErr
		}
		n, readErr := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for _, rule := range credentialRules {
				if rule.Pattern.Match(buf) {
					return rule, true, nil
				}
			}
			// Carry the tail forward so a pattern split across this boundary
			// is matched on the next pass.
			if len(buf) > scanOverlapBytes {
				buf = append(buf[:0], buf[len(buf)-scanOverlapBytes:]...)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return credentialpattern.Rule{}, false, nil
			}
			return credentialpattern.Rule{}, false, fmt.Errorf("scan export file: %w", readErr)
		}
	}
}
