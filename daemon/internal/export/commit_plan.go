package export

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// CommitPlanFilename is both the reserved top-level workspace path and the
// declared handoff member carrying the agent's opaque commit-plan proposal.
// The repo walk reserves this component, its case/normalization aliases, and
// every descendant beneath them; a near-prefix remains ordinary repository
// content.
const CommitPlanFilename = ".freeside-commit-plan.json"

var (
	commitPlanPathFold  = cases.Fold()
	commitPlanComponent = commitPlanPathFold.String(norm.NFC.String(CommitPlanFilename))
)

// IsCommitPlanNamespacePath reports whether p's first complete component is
// the reserved plan name under the reference checkout's case and Unicode
// normalization folding. A near-prefix remains ordinary content.
func IsCommitPlanNamespacePath(p string) bool {
	component, _, _ := strings.Cut(p, "/")
	return commitPlanPathFold.String(norm.NFC.String(component)) == commitPlanComponent
}

// DefaultMaxCommitPlanBytes keeps the opaque plan in the same intake class as
// manifest.json. A legitimate plan is much smaller; the generous bound avoids
// inventing a second practical limit while still bounding every later pass.
const DefaultMaxCommitPlanBytes int64 = 64 << 20

// resolveCommitPlan lifts the reserved plan as opaque bytes. It deliberately
// performs no JSON parsing and never consults workspace git state. Absence is
// benign, but any present irregular inode or over-cap file fails the export
// closed before manifest.json, the handoff commit marker, is written.
func resolveCommitPlan(fsys fs.FS, maxBytes int64) ([]byte, error) {
	info, err := fs.Lstat(fsys, CommitPlanFilename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("lstat commit plan: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("commit plan %q: %w", CommitPlanFilename, ErrCommitPlanNotRegular)
	}
	f, err := fsys.Open(CommitPlanFilename)
	if err != nil {
		return nil, fmt.Errorf("open commit plan: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := io.Reader(f)
	if maxBytes > 0 {
		r = io.LimitReader(f, maxBytes+1)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read commit plan: %w", err)
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("commit plan %q: %w", CommitPlanFilename, ErrCommitPlanTooLarge)
	}
	if int64(len(raw)) != info.Size() {
		return nil, fmt.Errorf("commit plan size %d became %d: %w", info.Size(), len(raw), ErrWorkspaceChanged)
	}
	return raw, nil
}

func writeCommitPlan(outDir string, raw []byte) error {
	if raw == nil {
		return nil
	}
	if err := os.WriteFile(filepath.Join(outDir, CommitPlanFilename), raw, 0o600); err != nil {
		return fmt.Errorf("write commit plan: %w", err)
	}
	return nil
}
