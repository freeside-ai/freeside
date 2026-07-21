package export

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
)

// readDirBatch is how many directory entries one ReadDir call may return.
// Directories are enumerated incrementally, never slurped whole: a hostile
// directory holding millions of names must trip the entry cap after at
// most one batch beyond it, not OOM the exporter first (fs.WalkDir reads
// and sorts entire directories before its callback runs, so it cannot be
// used here).
const readDirBatch = 512

// walkWorkspace classifies every entry of the read-only workspace into
// manifest entries, sorted bytewise by raw name. Regular entries come back
// without a digest; the blob step fills it from the single streamed read,
// and the final Manifest.Validate fails loud if any is left unset.
// maxEntries counts every walked name, files and directories alike, and
// fails the walk closed as soon as the workspace exceeds it (zero or
// negative disables the cap), so a hostile workspace of countless blobless
// entries (empty files, empty directories, symlinks, invalid names) cannot
// balloon exporter memory or the manifest before importer limits ever run;
// it also terminates a cyclic hostile filesystem.
//
// The walk never opens file content, never follows a symlink, and never
// descends into the workspace's own .git, a nested working tree, or a
// directory whose name is not representable — each becomes one recorded
// entry instead. Directories are otherwise not listed: like git, the
// manifest implies them through their children, so an empty directory is
// unrepresentable by design.
func walkWorkspace(fsys fs.FS, maxEntries int) ([]Entry, error) {
	w := &walker{fsys: fsys, maxEntries: maxEntries}
	if err := w.walkDir("."); err != nil {
		return nil, err
	}
	sort.Slice(w.entries, func(i, j int) bool {
		return string(w.entries[i].sortKey()) < string(w.entries[j].sortKey())
	})
	return w.entries, nil
}

type walker struct {
	fsys       fs.FS
	maxEntries int
	seen       int
	entries    []Entry
}

// walkDir enumerates one directory in bounded batches, records its
// non-directory children, and then recurses into the subdirectories that
// survive the skip rules.
func (w *walker) walkDir(dir string) error {
	f, err := w.fsys.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %q: %w", dir, err)
	}
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		_ = f.Close()
		return fmt.Errorf("directory %q does not support incremental reads", dir)
	}
	var subdirs []string
	for {
		batch, err := rd.ReadDir(readDirBatch)
		for _, de := range batch {
			descend, walkErr := w.visit(dir, de)
			if walkErr != nil {
				_ = f.Close()
				return walkErr
			}
			if descend != "" {
				subdirs = append(subdirs, descend)
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("read directory %q: %w", dir, err)
		}
	}
	_ = f.Close()
	for _, sd := range subdirs {
		if err := w.walkDir(sd); err != nil {
			return err
		}
	}
	return nil
}

// visit records one directory entry and reports the path to descend into,
// when there is one.
func (w *walker) visit(dir string, de fs.DirEntry) (string, error) {
	w.seen++
	if w.maxEntries > 0 && w.seen > w.maxEntries {
		return "", fmt.Errorf("at %q, cap %d: %w", path.Join(dir, de.Name()), w.maxEntries, ErrTooManyEntries)
	}
	p := path.Join(dir, de.Name())
	// One canonicality gate, shared with Entry.Validate: any name it
	// rejects (non-UTF-8, NUL) is recorded losslessly and, directory or
	// not, never descended into.
	if !validCanonicalPath(p) {
		w.entries = append(w.entries, Entry{PathHex: hex.EncodeToString([]byte(p)), Kind: EntryInvalidPath})
		return "", nil
	}
	// The workspace's own .git (directory, or file in a linked worktree)
	// is untrusted by design and recorded unexplored.
	if p == ".git" {
		w.entries = append(w.entries, Entry{Path: ".git", Kind: EntryGitDir})
		return "", nil
	}
	// The reserved evidence subtree is the agent's transient evidence-channel
	// staging; it leaves the workspace only through the evidence channel, never
	// the repo-change channel (plan §5.6), so the repo walk skips it entirely
	// and records nothing for it. The evidence emitter reaches it separately
	// through the declared descriptor.
	if p == EvidenceWorkspaceDir {
		return "", nil
	}
	// The reserved commit-plan namespace leaves only through its opaque handoff
	// member. Checking one complete path component, not a raw prefix, keeps a
	// near-prefix such as .freeside-commit-plan.json.bak ordinary content.
	if IsCommitPlanNamespacePath(p) {
		if p != CommitPlanFilename {
			return "", fmt.Errorf("reserved commit-plan path %q: %w", p, ErrCommitPlanPathAlias)
		}
		return "", nil
	}
	info, err := de.Info()
	if err != nil {
		return "", fmt.Errorf("lstat %q: %w", p, err)
	}
	if info.Mode().IsDir() {
		isSub, err := isSubmodule(w.fsys, p)
		if err != nil {
			return "", err
		}
		if isSub {
			w.entries = append(w.entries, Entry{Path: p, Kind: EntrySubmodule})
			return "", nil
		}
		return p, nil
	}
	entry, err := classifyFile(w.fsys, p, info)
	if err != nil {
		return "", err
	}
	w.entries = append(w.entries, entry)
	return "", nil
}

// isSubmodule reports whether the directory carries its own .git entry (a
// nested working tree: a gitlink checkout's directory, or a full clone).
// The probe is an lstat, so even a symlink named .git counts and is never
// followed.
func isSubmodule(fsys fs.FS, dir string) (bool, error) {
	if _, err := fs.Lstat(fsys, path.Join(dir, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("probe %q: %w", path.Join(dir, ".git"), err)
	}
	return true, nil
}

// classifyFile turns one non-directory entry into its manifest form. The
// lstat mode is the single authority, never DirEntry.Type(): a backend may
// report an unknown (zero) type for a non-regular entry, and an entry
// misread as regular would later be opened for hashing, which can follow a
// symlink or block on a FIFO. Only a mode proven regular is ever opened;
// anything else, including an entry the backend cannot classify, is
// recorded as special.
func classifyFile(fsys fs.FS, p string, info fs.FileInfo) (Entry, error) {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		// The target is recorded verbatim and never resolved; a target
		// whose bytes are not valid UTF-8 survives only best-effort in
		// JSON, which is acceptable because every symlink is
		// publish-blocking downstream regardless of target.
		target, err := fs.ReadLink(fsys, p)
		if err != nil {
			return Entry{}, fmt.Errorf("readlink %q: %w", p, err)
		}
		return Entry{Path: p, Kind: EntrySymlink, Target: &target}, nil
	case mode.IsRegular():
		if mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			return Entry{Path: p, Kind: EntryUnusualMode, Mode: ptrTo(unusualModeString(mode))}, nil
		}
		// Git normalization: only the owner-executable bit survives.
		normalized := "0644"
		if mode.Perm()&0o100 != 0 {
			normalized = "0755"
		}
		size := info.Size()
		return Entry{Path: p, Kind: EntryRegular, Mode: &normalized, Size: &size}, nil
	default:
		// FIFO, socket, device, irregular, or unclassifiable: recorded,
		// never opened.
		return Entry{Path: p, Kind: EntrySpecial}, nil
	}
}

// unusualModeString renders permission plus setuid/setgid/sticky bits in
// the five-digit octal form validUnusualMode accepts, e.g. "04755".
func unusualModeString(mode fs.FileMode) string {
	v := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		v |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		v |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		v |= 0o1000
	}
	return fmt.Sprintf("%05o", v)
}

func ptrTo[T any](v T) *T { return &v }
