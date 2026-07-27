package ward

import (
	"context"
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

// seedWorkspace puts the declared base into the workspace volume before
// anything else attaches it. A blank seed is a no-op.
//
// The shape is forced by the reference runtime, not chosen: Apple container
// 1.1.0 refuses to copy into a container that is not running, and silently
// discards a copy whose destination lies inside a mounted volume. So the gate
// starts a pinned seeder holding the workspace read-write, copies the checkout
// into the seeder's own root filesystem, and lets the seeder's fixed command
// move the tree onto the mount. The staged copy and the completion sentinel are
// two separate copies because a directory copy is not atomic.
//
// Nothing here is trusted afterwards. The copy's exit status, the seeder's exit
// status, and the fact that the seeder stopped are all statements about the
// seeding attempt, not about the volume; the observer reads the volume.
//
// The seeder is proven absent before returning, so its read-write attachment is
// provably released before the writer's own attach.
func (b *Backend) seedWorkspace(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) error {
	if hs.Seed.Mode != SeedBaseCheckout {
		return nil
	}
	if err := verifySeedSource(b.cfg, hs.Seed.SourceDir); err != nil {
		return err
	}
	readyDir, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-ready-")
	if err != nil {
		return failf(CheckWorkspaceSeeding, "create seed sentinel directory: %v", err)
	}
	defer os.RemoveAll(readyDir) //nolint:errcheck // best-effort cleanup of a host scratch dir holding no run output
	if err := os.WriteFile(filepath.Join(readyDir, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return failf(CheckWorkspaceSeeding, "write seed sentinel: %v", err)
	}

	spec := buildSeederSpec(b.cfg, hs, names, st.ownershipLabel)
	st.seeder.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckWorkspaceSeeding, "create seeder container: %v", err)
	}
	st.seeder.owned = true
	rep, err := b.rt.Inspect(ctx, names.Seeder)
	if err != nil {
		return failf(CheckWorkspaceSeeding, "inspect seeder before execution: %v", err)
	}
	// The fingerprint is captured only after the allowlist verified the
	// report's identity, as for the agent and exporter: a fingerprint bound to
	// the wrong object would make cleanup misclassify this run's own seeder.
	if err := verifySeedRoleAllowlist(b.cfg, rep, spec, names.Workspace, CheckWorkspaceSeeding); err != nil {
		return err
	}
	st.seeder.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seeder container %q: %v", names.Seeder, err)
	}
	if err := b.rt.StartContainer(ctx, names.Seeder); err != nil {
		return failf(CheckWorkspaceSeeding, "start seeder container: %v", err)
	}

	// Each copy is bounded on its own so a wedged transfer cannot consume the
	// whole handoff budget while the seeder holds the workspace read-write.
	if err := b.copyIntoSeeder(ctx, names.Seeder, hs.Seed.SourceDir, b.cfg.SeedStageDir); err != nil {
		return err
	}
	// Ordering signal, sent only after the staged tree is complete.
	if err := b.copyIntoSeeder(ctx, names.Seeder, readyDir, b.cfg.SeedReadyDir); err != nil {
		return err
	}

	if err := b.waitStopped(ctx, names.Seeder, st.seeder, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return failf(CheckWorkspaceSeeding, "seeder: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, names.Seeder); err != nil {
		return failf(CheckWorkspaceSeeding, "delete stopped seeder: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, names.Seeder, st.seeder, st.ownershipLabel, CheckWorkspaceSeeding); err != nil {
		return err
	}
	st.seeder = objectClaim{}
	return nil
}

func (b *Backend) copyIntoSeeder(ctx context.Context, id, hostDir, targetDir string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
	defer cancel()
	if err := b.rt.CopyIntoContainer(ctx, id, hostDir, targetDir); err != nil {
		// The reason names the destination, which is gate-generated
		// configuration, and never the host source path.
		return failf(CheckWorkspaceSeeding, "copy into seeder at %s: %v", targetDir, err)
	}
	return nil
}
