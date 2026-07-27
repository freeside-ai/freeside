package ward

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gitHeadPath is the file the observer later reads the seeded base from. A
// source without it is refused on the host rather than staged and then found
// wanting two VMs later.
const gitHeadPath = ".git/HEAD"

// verifySeedSource proves, on the host and before any runtime call, that dir is
// a seed source this gate is willing to copy into a workspace the writer will
// hold read-write.
//
// Everything here is about what the *daemon* stages, not about what the guest
// then does with it. The source is copied into a volume that a
// credential-bearing VM mounts read-write, so the gate refuses anything it
// cannot describe exactly:
//
//   - containment: dir and SeedRoot are both resolved through symlinks before
//     the prefix test, so a symlinked path cannot name a directory outside the
//     daemon's own checkout root;
//   - no symlinks anywhere in the tree, of any kind. publish.Transport's
//     FetchBase leaves none, so refusing them costs an honest source nothing,
//     while a symlink copied onto the workspace is a path trick aimed at
//     whatever later walks that tree;
//   - regular files and directories only, so no device node, socket, or FIFO
//     is staged;
//   - bounded bytes and entries, because the tree lands twice (the seeder's
//     root filesystem and then the workspace volume).
//
// The returned error is a CheckWorkspaceSeeding ConformanceFailure: by the time
// the gate is reading the filesystem, a bad source is a failed seeding
// assertion, not the syntactic caller error WorkspaceSeed.validate reports.
func verifySeedSource(cfg Config, dir string) error {
	if cfg.SeedRoot == "" {
		return failf(CheckWorkspaceSeeding, "backend is not configured with a seed root")
	}
	root, err := filepath.EvalSymlinks(cfg.SeedRoot)
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed root could not be resolved")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed source could not be resolved")
	}
	// Compare resolved against resolved: an unresolved prefix test would accept
	// a source whose own path is inside the root but whose real location is not.
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return failf(CheckWorkspaceSeeding, "seed source does not resolve under the daemon's seed root")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed source could not be read")
	}
	if !info.IsDir() {
		return failf(CheckWorkspaceSeeding, "seed source is not a directory")
	}

	var bytes int64
	var entries int
	walkErr := filepath.WalkDir(resolved, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return failf(CheckWorkspaceSeeding, "seed source could not be walked")
		}
		entries++
		if cfg.MaxSeedEntries > 0 && entries > cfg.MaxSeedEntries {
			return failf(CheckWorkspaceSeeding, "seed source exceeds the entry budget of %d", cfg.MaxSeedEntries)
		}
		switch {
		case d.IsDir():
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			// Named separately from the catch-all below because it is the case
			// an honest checkout could plausibly hit, and a reader debugging a
			// refusal should not have to guess which irregular kind it was.
			return failf(CheckWorkspaceSeeding, "seed source contains a symbolic link")
		case !d.Type().IsRegular():
			return failf(CheckWorkspaceSeeding, "seed source contains a non-regular file")
		}
		fi, err := d.Info()
		if err != nil {
			return failf(CheckWorkspaceSeeding, "seed source entry could not be sized")
		}
		bytes += fi.Size()
		if cfg.MaxSeedBytes > 0 && bytes > cfg.MaxSeedBytes {
			return failf(CheckWorkspaceSeeding, "seed source exceeds the byte budget of %d", cfg.MaxSeedBytes)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	// The observer reads the seeded base from this file; a source without it
	// could only ever produce a failed attestation.
	head, err := os.Lstat(filepath.Join(resolved, filepath.FromSlash(gitHeadPath)))
	if err != nil || !head.Mode().IsRegular() {
		return failf(CheckWorkspaceSeeding, "seed source does not carry a regular %s", gitHeadPath)
	}
	return nil
}
