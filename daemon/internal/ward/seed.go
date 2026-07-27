package ward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
func verifySeedSource(cfg Config, dir string) (resolvedDir, digest string, err error) {
	if cfg.SeedRoot == "" {
		return "", "", failf(CheckWorkspaceSeeding, "backend is not configured with a seed root")
	}
	root, err := filepath.EvalSymlinks(cfg.SeedRoot)
	if err != nil {
		return "", "", failf(CheckWorkspaceSeeding, "seed root could not be resolved")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", failf(CheckWorkspaceSeeding, "seed source could not be resolved")
	}
	// Compare resolved against resolved: an unresolved prefix test would accept
	// a source whose own path is inside the root but whose real location is not.
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", "", failf(CheckWorkspaceSeeding, "seed source does not resolve under the daemon's seed root")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "", failf(CheckWorkspaceSeeding, "seed source could not be read")
	}
	if !info.IsDir() {
		return "", "", failf(CheckWorkspaceSeeding, "seed source is not a directory")
	}

	var bytes int64
	var entries int
	var worktreeFiles int
	var lines []string
	var execPaths []string
	walkErr := filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return failf(CheckWorkspaceSeeding, "seed source could not be walked")
		}
		entries++
		if cfg.MaxSeedEntries > 0 && entries > cfg.MaxSeedEntries {
			return failf(CheckWorkspaceSeeding, "seed source exceeds the entry budget of %d", cfg.MaxSeedEntries)
		}
		rel, relErr := filepath.Rel(resolved, p)
		if relErr != nil {
			return failf(CheckWorkspaceSeeding, "seed source entry could not be located")
		}
		// A newline in a path would make the guest's line-oriented digest
		// ambiguous, and no git checkout needs one. Refused rather than
		// escaped, like every other delimiter this package meets.
		if strings.ContainsAny(rel, "\n\r") {
			return failf(CheckWorkspaceSeeding, "seed source contains a path with a line break")
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
		if !isUnderGitDir(rel) {
			worktreeFiles++
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return failf(CheckWorkspaceSeeding, "seed source entry could not be digested")
		}
		lines = append(lines, sum+"  ./"+filepath.ToSlash(rel))
		// The user-execute bit only, which is the one a git tree records.
		if fi.Mode().Perm()&0o100 != 0 {
			execPaths = append(execPaths, "./"+filepath.ToSlash(rel))
		}
		return nil
	})
	if walkErr != nil {
		return "", "", walkErr
	}
	// The observer reads the seeded base from this file; a source without it
	// could only ever produce a failed attestation.
	head, err := os.Lstat(filepath.Join(resolved, filepath.FromSlash(gitHeadPath)))
	if err != nil || !head.Mode().IsRegular() {
		return "", "", failf(CheckWorkspaceSeeding, "seed source does not carry a regular %s", gitHeadPath)
	}
	// A checkout with a .git but no working tree is the case the intended
	// producer actually hands over: publish.Transport.FetchBase moves HEAD to
	// the base and never checks anything out. Copying it would give the writer
	// an empty workspace that still carries the declared HEAD, so a
	// HEAD-only attestation would report it seeded at the exact base. Refuse it
	// here, where the reason can say what is missing.
	if worktreeFiles == 0 {
		return "", "", failf(CheckWorkspaceSeeding, "seed source carries no working-tree content, only a git directory")
	}
	return resolved, treeDigest(lines, execPaths), nil
}

// isUnderGitDir reports whether a source-relative path is the repository's own
// git directory rather than working-tree content.
func isUnderGitDir(rel string) bool {
	return rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator))
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // a path the gate is in the middle of validating, under its own seed root
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// treeDigest reduces the tree to one digest over two dimensions git itself
// distinguishes: every file's path and bytes, and which files are executable.
// Each dimension is hashed from bytewise-sorted lines and the two results are
// hashed together, exactly as the guest assembles them, so the host and the
// observer compute the same value from the same tree without either running
// git.
//
// The executable bit is in scope because a git tree records it (100755 versus
// 100644), so a workspace whose scripts lost it is not the approved tree even
// with identical bytes. Verified on the reference runtime that `container
// copy` and the seeder's `cp -a` preserve modes end to end, so including it
// cannot make an honest seed fail.
//
// Ownership is deliberately out of scope: files cross the host boundary owned
// by the host uid rather than the source's, so a uid-sensitive digest could
// never match. Directory modes are out of scope because git does not track
// them.
func treeDigest(contentLines, execPaths []string) string {
	return hex.EncodeToString(sha256Sum(
		digestLines(contentLines) + "\n" + digestLines(execPaths) + "\n"))
}

// digestLines hashes bytewise-sorted lines, each newline-terminated: the shape
// `... | sort | sha256sum` produces in the guest.
func digestLines(lines []string) string {
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
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
	// Everything below stages `source`, the path verification resolved, never
	// the caller's spelling of it. Verifying one path and copying another would
	// leave the containment check advisory: a symlink swapped in after the walk
	// would re-point the copy at a tree nothing checked, and outside SeedRoot,
	// which is the property the resolve exists to establish.
	source, digest, err := verifySeedSource(b.cfg, hs.Seed.SourceDir)
	if err != nil {
		return err
	}
	// The observer recomputes this over the volume, so the attestation covers
	// the tree that landed and not merely the HEAD pointer that came with it.
	st.seedTreeDigest = digest
	readyDir, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-ready-")
	if err != nil {
		return failf(CheckWorkspaceSeeding, "create seed sentinel directory: %v", err)
	}
	// The sentinel directory is the gate's own, made fresh under the host temp
	// root, so it needs no containment check; it carries one file the gate
	// wrote.
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
	if err := b.copyIntoSeeder(ctx, names.Seeder, source, b.cfg.SeedStageDir); err != nil {
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

// observeSeededBase reads the base the workspace actually holds and checks it
// against the one the caller declared, returning the observed value.
//
// This is the whole point of the unit. Everything before it is an attempt; this
// is the evidence. The observation is taken by a different VM than the one that
// wrote the workspace, after that VM was proven absent, through a read-only
// mount, and it reaches the host as bytes in the observer's own exported root
// filesystem rather than as anything the runtime says. A blank seed has no
// declared base and is not observed.
//
// The observer runs before the writer, because the base is a pre-writer fact:
// once the agent runs it may legitimately move HEAD, and an observation taken
// afterwards would attest the agent's work rather than the base it started from.
func (b *Backend) observeSeededBase(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) (string, error) {
	if hs.Seed.Mode != SeedBaseCheckout {
		return "", nil
	}
	spec := buildObserverSpec(b.cfg, hs, names, st.ownershipLabel)
	st.observer.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return "", failf(CheckObservedBaseIdentity, "create base observer container: %v", err)
	}
	st.observer.owned = true
	rep, err := b.rt.Inspect(ctx, names.Observer)
	if err != nil {
		return "", failf(CheckObservedBaseIdentity, "inspect base observer before execution: %v", err)
	}
	if err := verifySeedRoleAllowlist(b.cfg, rep, spec, names.Workspace, CheckObservedBaseIdentity); err != nil {
		return "", err
	}
	st.observer.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return "", failf(CheckObservedBaseIdentity, "base observer container %q: %v", names.Observer, err)
	}
	if err := b.rt.StartContainer(ctx, names.Observer); err != nil {
		return "", failf(CheckObservedBaseIdentity, "start base observer container: %v", err)
	}
	if err := b.waitStopped(ctx, names.Observer, st.observer, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return "", failf(CheckObservedBaseIdentity, "base observer: %v", err)
	}

	observed, err := b.readBaseProof(ctx, hs.RunID, names.Observer, st)
	if err != nil {
		return "", err
	}

	if err := b.rt.DeleteContainer(ctx, names.Observer); err != nil {
		return "", failf(CheckObservedBaseIdentity, "delete stopped base observer: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, names.Observer, st.observer, st.ownershipLabel, CheckObservedBaseIdentity); err != nil {
		return "", err
	}
	st.observer = objectClaim{}

	if observed != hs.Seed.Base.BaseSHA {
		// Categorical: the observed value comes from an archive nothing has
		// scanned, so neither side of the comparison is echoed.
		return "", failf(CheckObservedBaseIdentity, "workspace holds a different base than the one declared")
	}
	return observed, nil
}

// readBaseProof collects the observer's proof out of its stopped root
// filesystem and validates it. The archive is streamed under the same byte cap
// as the export path and removed as soon as the proof is read: it is evidence
// for this decision, never output.
func (b *Backend) readBaseProof(ctx context.Context, runID, id string, st *runState) (string, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+runID+"-base-")
	if err != nil {
		return "", failf(CheckObservedBaseIdentity, "create base proof directory: %v", err)
	}
	st.baseArchiveDir = dir
	defer func() {
		_ = os.RemoveAll(dir) // best-effort; the deferred teardown removes it again
		st.baseArchiveDir = ""
	}()
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(ctx, id, tarPath, CheckObservedBaseIdentity); err != nil {
		return "", err
	}
	f, err := os.Open(tarPath) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return "", failf(CheckObservedBaseIdentity, "open base proof archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle on a temp file removed above
	data, found, err := extractArchiveRegularFile(f, b.cfg.BaseProofPath, maxBaseProofBytes)
	if err != nil {
		return "", failf(CheckObservedBaseIdentity, "read base proof from observer rootfs: %v", err)
	}
	if !found {
		return "", failf(CheckObservedBaseIdentity, "base observer produced no proof")
	}
	return verifyBaseProof(data, st.ownershipLabel.Value, st.seedTreeDigest)
}

// maxBaseProofBytes bounds the proof read into the daemon's heap. The honest
// proof is four short lines; this sits far above that and far below anything
// that could pressure the host.
const maxBaseProofBytes = 4 << 10

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
