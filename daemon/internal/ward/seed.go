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
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// gitHeadPath is the file the observer later reads the seeded base from. A
// source without it is refused on the host rather than staged and then found
// wanting two VMs later.
const gitHeadPath = ".git/HEAD"

// maxGitHeadBytes is one full SHA-1 commit plus Git's terminating newline.
// The observer's shell reads HEAD through command substitution, so the host
// must reject a corrupted multi-megabyte file before it reaches that VM.
const maxGitHeadBytes = 41

// stageSeedSource materializes a private, gate-owned snapshot of the seed
// source and returns the digest of what it wrote.
//
// Verifying the caller's directory and then staging from it left a window: a
// concurrent mutation between the two could add a symlink, an empty directory,
// or bytes past the budget, and the copy would carry them. Widening the
// attestation caught each variant after the fact; copying first and verifying
// what was copied removes the window instead, because nothing else can touch
// the snapshot and the digest is computed from the bytes actually written.
//
// Everything here is about what the *daemon* stages, not about what the guest
// then does with it. The source is copied into a volume that a
// credential-bearing VM mounts read-write, so the gate refuses anything it
// cannot describe exactly:
//
//   - containment: SeedRoot is opened first and the source is opened through
//     that descriptor, so a swapped path cannot redirect either handle outside
//     the daemon's own checkout root;
//   - no symlinks anywhere in the tree, of any kind. publish.Transport's
//     FetchBase leaves none, so refusing them costs an honest source nothing,
//     while support for the symlinks git legitimately tracks remains deferred
//     to #339 because the reference runtime silently drops escaping links;
//   - regular files and directories only, so no device node, socket, or FIFO
//     is staged;
//   - local Git config reduced to validated daemon-authored facts, so neither
//     hostile values nor ignored comments cross into the writer;
//   - bounded bytes and entries, because the tree lands twice (the seeder's
//     root filesystem and then the workspace volume).
//
// The returned error is a CheckWorkspaceSeeding ConformanceFailure: by the time
// the gate is reading the filesystem, a bad source is a failed seeding
// assertion, not the syntactic caller error WorkspaceSeed.validate reports.
func stageSeedSource(cfg Config, dir, declaredRepo string, declaredRepositoryID int64, snapshot string) (digest string, err error) {
	if cfg.SeedRoot == "" {
		return "", failf(CheckWorkspaceSeeding, "backend is not configured with a seed root")
	}
	// Anchor on SeedRoot's own descriptor first, then open the source THROUGH
	// it. Resolving names and comparing strings left the anchor itself
	// acquired by pathname: the source could be swapped for a symlink after the
	// containment test and before the open, and the root descriptor would land
	// outside SeedRoot entirely. A descriptor cannot be swapped, and every
	// component of the path opened through it is validated against it, so
	// containment stops being a check that can go stale and becomes a property
	// of the handle.
	anchor, err := os.OpenRoot(cfg.SeedRoot)
	if err != nil {
		return "", failf(CheckWorkspaceSeeding, "seed root could not be opened")
	}
	defer anchor.Close() //nolint:errcheck // read-only handle
	rel, err := filepath.Rel(cfg.SeedRoot, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		// Lexically outside; the descriptor below would refuse it too, but the
		// reason is clearer said here.
		return "", failf(CheckWorkspaceSeeding, "seed source does not resolve under the daemon's seed root")
	}
	srcRoot, err := anchor.OpenRoot(filepath.ToSlash(rel))
	if err != nil {
		// Escaping the anchor, absent, or not a directory all land here; none of
		// them is a source the gate will stage, and distinguishing them would
		// report the daemon's layout into a ConformanceFailure.Reason.
		return "", failf(CheckWorkspaceSeeding, "seed source could not be opened under the daemon's seed root")
	}
	defer srcRoot.Close() //nolint:errcheck // read-only handle

	remaining := cfg.MaxSeedBytes
	var entries, gitConfigContentIndex int
	var gitConfigSourceBytes int64
	gitConfigContentIndex = -1
	var contentLines, execPaths, dirPaths []string
	walkErr := fs.WalkDir(srcRoot.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return failf(CheckWorkspaceSeeding, "seed source could not be walked")
		}
		entries++
		if cfg.MaxSeedEntries > 0 && entries > cfg.MaxSeedEntries {
			return failf(CheckWorkspaceSeeding, "seed source exceeds the entry budget of %d", cfg.MaxSeedEntries)
		}
		// A newline in a path would make the guest's line-oriented digest
		// ambiguous, and no git checkout needs one. Refused rather than
		// escaped, like every other delimiter this package meets.
		if strings.ContainsAny(rel, "\n\r") {
			return failf(CheckWorkspaceSeeding, "seed source contains a path with a line break")
		}
		dest := filepath.Join(snapshot, filepath.FromSlash(rel))
		switch {
		case d.IsDir():
			if rel != "." {
				if mkErr := os.Mkdir(dest, 0o700); mkErr != nil {
					return failf(CheckWorkspaceSeeding, "seed snapshot directory could not be created")
				}
			}
			// Directories are digested too: an empty one is invisible in a
			// content-only digest, and an injected .git/rebase-apply changes
			// git's behaviour without changing a single file.
			dirPaths = append(dirPaths, findPath(rel))
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			// Named separately from the catch-all below because it is the case
			// an honest checkout could plausibly hit, and a reader debugging a
			// refusal should not have to guess which irregular kind it was.
			return failf(CheckWorkspaceSeeding, "seed source contains a symbolic link")
		case !d.Type().IsRegular():
			return failf(CheckWorkspaceSeeding, "seed source contains a non-regular file")
		}
		// Copy and hash in one pass, bounded as it goes. The budget applies to
		// bytes actually written, not to a size read before the copy, so a file
		// growing underneath cannot spend more than the cap allows. The mode
		// comes from the opened descriptor, not from the walk entry, for the
		// same reason the open refuses to follow: the entry describes what was
		// there at walk time.
		sum, written, perm, copyErr := copySeedFile(srcRoot, rel, dest, remaining)
		if copyErr != nil {
			return copyErr
		}
		remaining -= written
		contentLine := sum + "  " + findPath(rel)
		if filepath.ToSlash(rel) == ".git/config" {
			gitConfigContentIndex = len(contentLines)
			gitConfigSourceBytes = written
		}
		contentLines = append(contentLines, contentLine)
		// The user-execute bit only, which is the one a git tree records.
		if perm&0o100 != 0 {
			execPaths = append(execPaths, findPath(rel))
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	// The observer reads the seeded base from this file; a source without it
	// could only ever produce a failed attestation.
	head, err := os.Lstat(filepath.Join(snapshot, filepath.FromSlash(gitHeadPath)))
	if err != nil || !head.Mode().IsRegular() {
		return "", failf(CheckWorkspaceSeeding, "seed source does not carry a regular %s", gitHeadPath)
	}
	if head.Size() > maxGitHeadBytes {
		return "", failf(CheckWorkspaceSeeding, "seed source carries an oversized %s", gitHeadPath)
	}
	// Do not infer materialization from the presence of an ordinary worktree
	// file. A legitimately empty commit has none. The read-only observer
	// compares the raw files and modes with HEAD and distinguishes that valid
	// case from an unmaterialized checkout of a non-empty commit.
	// Bound to the SNAPSHOT, not the source it came from. Checking the mutable
	// source would leave the same window the snapshot exists to close: a
	// checkout swapped mid-walk could put another repository into the snapshot,
	// and its digest and HEAD would be self-consistent about the wrong thing.
	if err := canonicalizeSeedRepoBinding(snapshot, declaredRepo, declaredRepositoryID); err != nil {
		return "", err
	}
	if gitConfigContentIndex < 0 {
		return "", failf(CheckWorkspaceSeeding, "seed source carries no git config to bind it to a repository")
	}
	configPath := filepath.Join(snapshot, ".git", "config")
	configInfo, err := os.Stat(configPath)
	if err != nil || !configInfo.Mode().IsRegular() {
		return "", failf(CheckWorkspaceSeeding, "canonical seed git config could not be examined")
	}
	if growth := configInfo.Size() - gitConfigSourceBytes; growth > remaining {
		return "", failf(CheckWorkspaceSeeding, "canonical seed git config exceeds the byte budget")
	}
	configSum, err := fileSHA256(configPath)
	if err != nil {
		return "", failf(CheckWorkspaceSeeding, "canonical seed git config could not be hashed")
	}
	contentLines[gitConfigContentIndex] = configSum + "  ./.git/config"
	return treeDigest(contentLines, execPaths, dirPaths), nil
}

// findPath renders a source-relative path the way `find .` prints it in the
// guest, so the host and the observer sort and hash identical strings.
func findPath(rel string) string {
	if rel == "." {
		return "."
	}
	return "./" + filepath.ToSlash(rel)
}

// copySeedFile copies one regular file into the snapshot, hashing the bytes it
// writes and refusing to write more than budget. Hashing what is written, not
// what was read from a separate pass, is what makes the digest describe the
// snapshot exactly.
//
// The open refuses to follow a symlink, and the type and permissions come from
// the opened descriptor rather than the walk's directory entry. That entry
// describes what was there when the walk passed it; an entry replaced by a
// symlink in between would otherwise be followed, and the target's bytes, from
// anywhere on the host, would be stored as a regular file whose digest and
// irregular=absent report are perfectly self-consistent about the wrong thing.
func copySeedFile(root *os.Root, rel, dest string, budget int64) (sum string, written int64, perm fs.FileMode, err error) {
	in, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed source entry could not be opened without following a link")
	}
	defer in.Close() //nolint:errcheck // read-only handle
	st, err := in.Stat()
	if err != nil {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed source entry could not be examined")
	}
	if !st.Mode().IsRegular() {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed source entry is not a regular file")
	}
	perm = st.Mode().Perm()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // gate-owned snapshot under a fresh temp directory
	if err != nil {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed snapshot entry could not be created")
	}
	h := sha256.New()
	// One byte past the budget is enough to know the source outgrew it.
	written, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, budget+1))
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed source entry could not be staged")
	}
	if closeErr != nil {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed snapshot entry could not be closed")
	}
	if written > budget {
		return "", 0, 0, failf(CheckWorkspaceSeeding, "seed source exceeds the byte budget")
	}
	return hex.EncodeToString(h.Sum(nil)), written, perm, nil
}

// The daemon-authored marks the checkout materialization boundary stamps name
// the managed repository in local config and its canonical forge ID in a
// daemon metadata file. Ward re-gates against them without importing the
// publishing lane; the repository key literal is duplicated deliberately and
// a change to it must move both.
const (
	seedRepoBindingSectionName = "freeside"
	seedRepoBindingSubsection  = "transport"
	seedRepoBindingName        = "repo"
	seedRepoBindingIDPath      = ".git/freeside-repository-id"
	seedWorkerGitName          = "Freeside Worker"
	seedWorkerGitEmail         = "worker@freeside.invalid"
	maxGitConfigBytes          = 1 << 20
	maxRepositoryIDBytes       = 64
)

// seedGitConfigKeys is the safe subset of publish.pristineConfigKeys that can
// describe an ordinary daemon-authored working tree. Ward cannot import the
// publishing lane, so these literals are duplicated deliberately and changes
// to their shared keys must move both. core.worktree is intentionally absent:
// FetchBase initializes .git inside its checkout and never authors that key,
// while accepting an arbitrary path would redirect the writer's worktree.
// user.* is likewise absent: canonicalSeedGitConfig appends the fixed worker
// identity only after every source-provided key and value has passed this gate.
var seedGitConfigKeys = map[string]bool{
	"core.bare":                    true,
	"core.filemode":                true,
	"core.ignorecase":              true,
	"core.logallrefupdates":        true,
	"core.precomposeunicode":       true,
	"core.repositoryformatversion": true,
	"core.symlinks":                true,
	"extensions.objectformat":      true,
	"freeside.transport.repo":      true,
}

var seedGitConfigKeyOrder = []string{
	"core.repositoryformatversion",
	"core.filemode",
	"core.bare",
	"core.logallrefupdates",
	"core.symlinks",
	"core.ignorecase",
	"core.precomposeunicode",
	"extensions.objectformat",
	"freeside.transport.repo",
}

// canonicalizeSeedRepoBinding proves the checkout was materialized from the
// repository the caller declared, then replaces its local config with the
// validated daemon-authored facts and the fixed worker commit identity in one
// canonical, comment-free form.
//
// Containment under SeedRoot is not enough on its own. A seed root holds
// checkouts for every managed repository, and forks share commits, so a source
// naming another repository whose HEAD happens to equal the declared SHA would
// satisfy every other check here: the digest is computed from that same source,
// so the observer would agree with it, and the writer would receive the wrong
// repository's entire object database under a correct-looking base.
//
// The config is read as text rather than through git, which ward may not shell
// out to. Anything ambiguous is refused rather than resolved: an include can
// define a key elsewhere, a repeated key has no single answer, and even an
// allowlisted key can hide credential bytes in its value. Full-line comments
// and formatting are discarded when the canonical file is written, so no
// ignored input reaches the credential-bearing writer.
func canonicalizeSeedRepoBinding(dir, declaredRepo string, declaredRepositoryID int64) error {
	configPath := filepath.Join(dir, ".git", "config")
	f, err := os.Open(configPath) //nolint:gosec // inside the gate-owned snapshot
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed source carries no readable git config to bind it to a repository")
	}
	defer f.Close() //nolint:errcheck // read-only handle
	// Bounded at read time, not after: reading first and measuring second would
	// let a corrupted source pull hundreds of megabytes into the daemon before
	// the budget it is supposed to be held to ever applies. One byte past the
	// cap is enough to know it is over.
	data, err := io.ReadAll(io.LimitReader(f, maxGitConfigBytes+1))
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed source git config could not be read")
	}
	if len(data) > maxGitConfigBytes {
		return failf(CheckWorkspaceSeeding, "seed source git config exceeds the readable budget")
	}
	text := string(data)
	// An include can define the binding in a file this check never reads, so a
	// config carrying one cannot be evaluated here at all.
	if strings.Contains(strings.ToLower(text), "[include") {
		return failf(CheckWorkspaceSeeding, "seed source git config carries an include directive")
	}

	values := make(map[string]string)
	var section, subsection string
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(t, "[") {
			// Git removes backslashes while parsing quoted subsection names:
			// [freeside "trans\port"] therefore aliases "transport". The
			// daemon-authored config needs no escaped section header at all, so
			// refuse one rather than implementing only the latest alias.
			if end := strings.IndexByte(t, ']'); end >= 0 && strings.Contains(t[:end+1], `\`) {
				return failf(CheckWorkspaceSeeding, "seed source git config carries an escaped section header")
			}
			var ok bool
			section, subsection, ok = parseSeedGitConfigSection(t)
			if !ok {
				return failf(CheckWorkspaceSeeding, "seed source git config carries a malformed section header")
			}
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		name, value, ok := strings.Cut(t, "=")
		name = strings.TrimSpace(name)
		key := strings.ToLower(section)
		if subsection != "" {
			key += "." + subsection
		}
		key += "." + strings.ToLower(name)
		if !seedGitConfigKeys[key] {
			// Never echo the rejected key or value: either can contain
			// credential material in a corrupted checkout.
			return failf(CheckWorkspaceSeeding, "seed source git config carries a non-daemon key")
		}
		if !ok {
			return failf(CheckWorkspaceSeeding, "seed source git config carries an implicit or missing value")
		}
		value = strings.TrimSpace(value)
		if _, duplicate := values[key]; duplicate {
			return failf(CheckWorkspaceSeeding, "seed source git config carries a repeated key")
		}
		if !validSeedGitConfigValue(key, value, declaredRepo) {
			return failf(CheckWorkspaceSeeding, "seed source git config carries a non-daemon value")
		}
		values[key] = value
	}

	repoKey := seedRepoBindingSectionName + "." + seedRepoBindingSubsection + "." + seedRepoBindingName
	if _, ok := values[repoKey]; !ok {
		return failf(CheckWorkspaceSeeding, "seed source git config does not carry exactly one repository binding")
	}
	idFile, err := os.Open(filepath.Join(dir, filepath.FromSlash(seedRepoBindingIDPath))) //nolint:gosec // inside the verified snapshot
	if err != nil {
		return failf(CheckWorkspaceSeeding, "seed source carries no readable canonical repository id binding")
	}
	defer idFile.Close() //nolint:errcheck // read-only handle
	idData, err := io.ReadAll(io.LimitReader(idFile, maxRepositoryIDBytes+1))
	if err != nil || len(idData) > maxRepositoryIDBytes {
		return failf(CheckWorkspaceSeeding, "seed source canonical repository id binding could not be read")
	}
	idText := strings.TrimSuffix(string(idData), "\n")
	repositoryID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || repositoryID <= 0 || idText != strconv.FormatInt(repositoryID, 10) {
		return failf(CheckWorkspaceSeeding, "seed source carries an invalid canonical repository id binding")
	}
	if repositoryID != declaredRepositoryID {
		return failf(CheckWorkspaceSeeding, "seed source is bound to a different repository identity than the declared base")
	}
	if err := os.WriteFile(configPath, []byte(canonicalSeedGitConfig(values)), 0o600); err != nil {
		return failf(CheckWorkspaceSeeding, "canonical seed git config could not be written")
	}
	return nil
}

func validSeedGitConfigValue(key, value, declaredRepo string) bool {
	switch key {
	case "core.repositoryformatversion":
		return value == "0"
	case "core.bare":
		return value == "false"
	case "core.logallrefupdates":
		return value == "true"
	case "extensions.objectformat":
		return value == "sha1"
	case "core.filemode", "core.ignorecase", "core.precomposeunicode", "core.symlinks":
		return value == "true" || value == "false"
	case seedRepoBindingSectionName + "." + seedRepoBindingSubsection + "." + seedRepoBindingName:
		return value == declaredRepo && !strings.ContainsAny(value, "\"\\")
	}
	return false
}

func canonicalSeedGitConfig(values map[string]string) string {
	var b strings.Builder
	section := ""
	for _, key := range seedGitConfigKeyOrder {
		value, ok := values[key]
		if !ok {
			continue
		}
		keySection, name, _ := strings.Cut(key, ".")
		if key == seedRepoBindingSectionName+"."+seedRepoBindingSubsection+"."+seedRepoBindingName {
			keySection = seedRepoBindingSectionName + ` "` + seedRepoBindingSubsection + `"`
			name = seedRepoBindingName
		}
		if keySection != section {
			b.WriteByte('[')
			b.WriteString(keySection)
			b.WriteString("]\n")
			section = keySection
		}
		b.WriteByte('\t')
		b.WriteString(name)
		b.WriteString(" = ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
	// The worker may author disposable checkpoint history, but the fetched
	// checkout may not choose who authored it. Inject the identity after source
	// validation rather than admitting user.* into seedGitConfigKeys.
	b.WriteString("[user]\n\tname = ")
	b.WriteString(seedWorkerGitName)
	b.WriteString("\n\temail = ")
	b.WriteString(seedWorkerGitEmail)
	b.WriteByte('\n')
	return b.String()
}

// parseSeedGitConfigSection recognizes the section forms accepted from the
// daemon-authored config. Git folds section names, preserves modern quoted
// subsection case, and folds the deprecated dotted subsection spelling.
func parseSeedGitConfigSection(line string) (section, subsection string, ok bool) {
	if len(line) < 3 || line[0] != '[' {
		return "", "", false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", "", false
	}
	if trailing := strings.TrimSpace(line[end+1:]); trailing != "" &&
		!strings.HasPrefix(trailing, "#") && !strings.HasPrefix(trailing, ";") {
		return "", "", false
	}
	inner := strings.TrimSpace(line[1:end])
	if inner == "" {
		return "", "", false
	}
	if split := strings.IndexFunc(inner, unicode.IsSpace); split >= 0 {
		section = strings.ToLower(inner[:split])
		quoted := strings.TrimSpace(inner[split:])
		if section == "" || len(quoted) < 3 || quoted[0] != '"' ||
			quoted[len(quoted)-1] != '"' || strings.ContainsRune(quoted[1:len(quoted)-1], '"') {
			return "", "", false
		}
		return section, quoted[1 : len(quoted)-1], true
	}
	section, subsection, dotted := strings.Cut(inner, ".")
	if section == "" || dotted && subsection == "" {
		return "", "", false
	}
	if dotted {
		subsection = strings.ToLower(subsection)
	}
	return strings.ToLower(section), subsection, true
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

// treeDigest reduces the tree to one digest over three dimensions: every
// file's path and bytes, which files are executable, and which directories
// exist. Directories are in scope because an empty one is invisible to a
// content-only digest, and an injected .git/rebase-apply changes git's
// behaviour without changing a single file.
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
func treeDigest(contentLines, execPaths, dirPaths []string) string {
	return hex.EncodeToString(sha256Sum(
		digestLines(contentLines) + "\n" + digestLines(execPaths) + "\n" + digestLines(dirPaths) + "\n"))
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
	// Everything below stages the private snapshot, never the caller's mutable
	// path. The source was opened through SeedRoot's descriptor and every byte
	// was copied through that anchored handle before this runtime boundary.
	snapshot, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-seed-")
	if err != nil {
		return failf(CheckWorkspaceSeeding, "create seed snapshot directory: %v", err)
	}
	st.seedSnapshotDir = snapshot
	digest, err := stageSeedSource(
		b.cfg, hs.Seed.SourceDir, hs.Seed.Base.Repo, hs.Seed.Base.RepositoryID, snapshot,
	)
	if err != nil {
		return err
	}
	source := snapshot
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
	if err := verifySeedRoleAllowlist(
		rep, spec, names.Workspace, b.cfg.WorkspaceTarget, CheckWorkspaceSeeding,
	); err != nil {
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
	if err := verifySeedRoleAllowlist(
		rep, spec, names.Workspace, b.cfg.WorkspaceTarget, CheckObservedBaseIdentity,
	); err != nil {
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
