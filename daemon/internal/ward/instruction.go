package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	instructionCompositionVersion = "claude_explicit_bundle_v2"
	instructionSourceAbsent       = "absent"
)

var errInstructionBudget = errors.New(
	"repository instructions exceed the bundle byte limit",
)

// errInstructionImportUnresolved marks an @import whose target simply is not
// a readable file in the snapshot. That is the ordinary-markdown case, not an
// attack: a decorator, a handle, a CSS at-rule, or a retina suffix anywhere in
// a managed repository's CLAUDE.md begins a line with "@" and names nothing.
// Such a line keeps its literal text. A target that exists but reaches outside
// the snapshot, through git metadata, through a symbolic link, or into a
// cycle, stays fatal: those are shapes prose does not produce by accident.
var errInstructionImportUnresolved = errors.New(
	"repository instruction import names no readable snapshot file",
)

type repositoryInstruction struct {
	path, digest string
	body         []byte
}

// composeClaudeInstructions turns the admitted host file and every
// path-scoped CLAUDE.md in ward's immutable exact-base snapshot into the one
// explicit safe-mode bundle the writer receives. The workspace is never read:
// it is a future writer surface, while seedSnapshotDir is the private tree
// stageSeedSource already copied, bounded, and hashed before a separate VM
// proved the workspace byte-for-byte identical.
func composeClaudeInstructions(
	hs HandoffSpec,
	st *runState,
) ([]byte, HandoffJournalInstructions, error) {
	if st.seedSnapshotDir == "" {
		return nil, HandoffJournalInstructions{}, failf(
			CheckControlPlaneIsolation,
			"Claude instruction composition has no trusted-base snapshot",
		)
	}
	snapshotRoot, err := os.OpenRoot(st.seedSnapshotDir)
	if err != nil {
		return nil, HandoffJournalInstructions{}, failf(
			CheckControlPlaneIsolation, "open trusted-base snapshot: %v", err,
		)
	}
	defer snapshotRoot.Close() //nolint:errcheck // read-only snapshot handle
	var sources []repositoryInstruction
	var repositoryBytes int64
	err = walkRoot(snapshotRoot, func(rel string, entry fs.DirEntry) (bool, error) {
		if entry.IsDir() {
			// Git metadata is skipped at any depth, not only at the
			// repository root. A vendored checkout or a committed fixture can
			// hold a nested .git, expansion refuses any path with a .git
			// component outright, and that refusal is fatal, so walking into
			// one would make composition (and therefore every run on that
			// repository) fail on ordinary repository content.
			if entry.Name() == ".git" {
				return false, nil
			}
			return true, nil
		}
		if entry.Name() != instructionFileName {
			return false, nil
		}
		body, err := expandRepositoryInstruction(
			snapshotRoot,
			rel,
			domain.MaxVendorInstructionBytes-repositoryBytes,
			map[string]bool{},
		)
		if err != nil {
			return false, err
		}
		repositoryBytes += int64(len(body))
		sum := sha256.Sum256(body)
		sources = append(sources, repositoryInstruction{
			path:   rel,
			digest: hex.EncodeToString(sum[:]),
			body:   body,
		})
		return false, nil
	})
	if err != nil {
		return nil, HandoffJournalInstructions{}, failf(
			CheckControlPlaneIsolation,
			"read trusted-base repository instructions: %v",
			err,
		)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].path < sources[j].path })

	manifestHash := sha256.New()
	var bundle bytes.Buffer
	bundle.WriteString("# Freeside Explicit Claude Instruction Bundle\n\n")
	bundle.WriteString(
		"Composition: " + instructionCompositionVersion + "\n\n" +
			"Apply each digest-delimited repository block only within its named " +
			"path scope. The deepest matching repository scope takes precedence " +
			"among repository blocks. Apply the final operator-host block " +
			"globally; it takes precedence over every repository block.\n\n" +
			"## Trusted-Base Repository Instructions\n",
	)
	for _, source := range sources {
		manifestHash.Write([]byte(source.path))
		manifestHash.Write([]byte{0})
		manifestHash.Write([]byte(source.digest))
		manifestHash.Write([]byte{0})
		scope := filepath.ToSlash(filepath.Dir(source.path))
		bundle.WriteString("\n### Scope ")
		bundle.WriteString(strconv.Quote(scope))
		bundle.WriteString("\n\n--- BEGIN REPOSITORY INSTRUCTION sha256:")
		bundle.WriteString(source.digest)
		bundle.WriteString(" ---\n")
		bundle.Write(source.body)
		if len(source.body) == 0 || source.body[len(source.body)-1] != '\n' {
			bundle.WriteByte('\n')
		}
		bundle.WriteString("--- END REPOSITORY INSTRUCTION sha256:")
		bundle.WriteString(source.digest)
		bundle.WriteString(" ---\n")
	}
	hostDigest := instructionSourceAbsent
	bundle.WriteString("\n## Operator-Host Instructions\n\n")
	if hs.Agent.VendorInstructions.Present {
		hostDigest = strings.TrimPrefix(
			string(hs.Agent.VendorInstructions.Digest),
			"sha256:",
		)
		bundle.WriteString("--- BEGIN OPERATOR-HOST INSTRUCTION sha256:")
		bundle.WriteString(hostDigest)
		bundle.WriteString(" ---\n")
		bundle.Write(hs.Agent.VendorInstructions.Body)
		if len(hs.Agent.VendorInstructions.Body) == 0 ||
			hs.Agent.VendorInstructions.Body[len(hs.Agent.VendorInstructions.Body)-1] != '\n' {
			bundle.WriteByte('\n')
		}
		bundle.WriteString("--- END OPERATOR-HOST INSTRUCTION sha256:")
		bundle.WriteString(hostDigest)
		bundle.WriteString(" ---\n")
	} else {
		bundle.WriteString("(No operator-host instruction file was admitted.)\n")
	}
	if int64(bundle.Len()) > domain.MaxVendorInstructionBytes {
		return nil, HandoffJournalInstructions{}, failf(
			CheckControlPlaneIsolation,
			"composed Claude instruction bundle is %d bytes, limit %d",
			bundle.Len(),
			domain.MaxVendorInstructionBytes,
		)
	}
	body := bytes.Clone(bundle.Bytes())
	bundleSum := sha256.Sum256(body)
	return body, HandoffJournalInstructions{
		CompositionVersion:       instructionCompositionVersion,
		HostDigest:               hostDigest,
		RepositoryManifestDigest: hex.EncodeToString(manifestHash.Sum(nil)),
		BundleDigest:             hex.EncodeToString(bundleSum[:]),
	}, nil
}

// expandRepositoryInstruction resolves standalone Claude @imports from the
// same private exact-base snapshot and inlines their bytes into the importing
// CLAUDE.md scope. The aggregate remaining budget flows through recursive
// reads, so rejected repositories never accumulate an unbounded source set in
// memory before the final bundle-size check. An import naming nothing
// readable in the snapshot keeps its line verbatim, because ordinary markdown
// starts lines with "@"; every hostile shape (escape, git metadata, symbolic
// link, cycle) still fails the whole composition closed.
func expandRepositoryInstruction(
	root *os.Root, rel string, remaining int64, stack map[string]bool,
) ([]byte, error) {
	if remaining < 0 {
		return nil, errInstructionBudget
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("repository instruction import escapes the trusted snapshot")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" {
			return nil, fmt.Errorf("repository instruction import enters git metadata")
		}
	}
	canonical := filepath.ToSlash(clean)
	if stack[canonical] {
		return nil, fmt.Errorf("repository instruction import cycle at %q", canonical)
	}
	stack[canonical] = true
	defer delete(stack, canonical)

	var rootedPath string
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		rootedPath = filepath.Join(rootedPath, part)
		info, err := root.Lstat(rootedPath)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("%w: %q", errInstructionImportUnresolved, canonical)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("repository instruction path contains a symbolic link")
		}
		last := index == len(parts)-1
		if (!last && !info.IsDir()) || (last && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("%w: %q is not a regular file",
				errInstructionImportUnresolved, canonical)
		}
	}
	file, err := root.OpenFile(rootedPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > remaining {
		return nil, errInstructionBudget
	}

	var expanded bytes.Buffer
	for len(body) > 0 {
		line := body
		if newline := bytes.IndexByte(body, '\n'); newline >= 0 {
			line, body = body[:newline+1], body[newline+1:]
		} else {
			body = nil
		}
		importName, imported := standaloneInstructionImport(line)
		if !imported {
			expanded.Write(line)
			continue
		}
		importRel := filepath.Join(filepath.Dir(clean), filepath.FromSlash(importName))
		importBody, err := expandRepositoryInstruction(
			root,
			filepath.ToSlash(importRel),
			remaining-int64(expanded.Len()),
			stack,
		)
		if errors.Is(err, errInstructionImportUnresolved) {
			// Ordinary prose that merely starts with "@". Keep the line
			// exactly as the repository wrote it rather than refusing to
			// compose, which would make the run unstartable.
			expanded.Write(line)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("expand %q from %q: %w", importName, canonical, err)
		}
		expanded.Write(importBody)
		if len(importBody) == 0 || importBody[len(importBody)-1] != '\n' {
			expanded.WriteByte('\n')
		}
		if int64(expanded.Len()) > remaining {
			return nil, errInstructionBudget
		}
	}
	return bytes.Clone(expanded.Bytes()), nil
}

func standaloneInstructionImport(line []byte) (string, bool) {
	trimmed := strings.TrimSpace(string(line))
	if len(trimmed) < 2 || trimmed[0] != '@' {
		return "", false
	}
	return strings.TrimSpace(trimmed[1:]), true
}

// seedVendorInstructions writes the exact explicit bundle into the run-owned
// instruction volume. State-free synthetic callers retain the legacy admitted
// host-only shape; a production Claude launch must use the deterministic
// host-plus-trusted-base composition above. The seeder is pinned,
// credential-free, and network-free; an independent read-only observer below
// proves what actually landed.
func (b *Backend) seedVendorInstructions(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) error {
	body := hs.Agent.VendorInstructions.Body
	present := hs.Agent.VendorInstructions.Present
	if hs.Agent.LaunchState == LaunchStateClaudeClean {
		composed, binding, err := composeClaudeInstructions(hs, st)
		if err != nil {
			return err
		}
		body, present = composed, true
		st.instructionBundleBody = bytes.Clone(composed)
		st.preparedInstructions = &binding
	}
	snapshot, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-instructions-")
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"create vendor-instruction snapshot directory: %v", err)
	}
	st.instructionSnapshotDir = snapshot
	if present {
		if err := os.WriteFile(
			filepath.Join(snapshot, instructionFileName),
			body,
			0o600,
		); err != nil {
			return failf(CheckControlPlaneIsolation,
				"write vendor-instruction snapshot: %v", err)
		}
	}
	readyDir, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-instruction-ready-")
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"create vendor-instruction sentinel directory: %v", err)
	}
	defer os.RemoveAll(readyDir) //nolint:errcheck // best-effort gate-owned scratch cleanup
	if err := os.WriteFile(
		filepath.Join(readyDir, seedReadyFile), []byte("ready\n"), 0o600,
	); err != nil {
		return failf(CheckControlPlaneIsolation,
			"write vendor-instruction sentinel: %v", err)
	}

	spec := buildInstructionSeederSpec(b.cfg, hs, names, st.ownershipLabel)
	st.instructionSeeder.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckControlPlaneIsolation,
			"create vendor-instruction seeder: %v", err)
	}
	st.instructionSeeder.owned = true
	rep, err := b.rt.Inspect(ctx, names.InstructionSeeder)
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"inspect vendor-instruction seeder before execution: %v", err)
	}
	if err := verifySeedRoleAllowlist(
		rep,
		spec,
		names.Instructions,
		instructionVolumeTarget,
		CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.instructionSeeder.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction seeder %q: %v", names.InstructionSeeder, err)
	}
	if err := b.rt.StartContainer(ctx, names.InstructionSeeder); err != nil {
		return failf(CheckControlPlaneIsolation,
			"start vendor-instruction seeder: %v", err)
	}
	if err := b.copyInstructionInput(
		ctx, names.InstructionSeeder, snapshot, instructionStageDir,
	); err != nil {
		return err
	}
	if err := b.copyInstructionInput(
		ctx, names.InstructionSeeder, readyDir, instructionReadyDir,
	); err != nil {
		return err
	}
	if err := b.waitStopped(
		ctx,
		names.InstructionSeeder,
		st.instructionSeeder,
		st.ownershipLabel,
		b.cfg.SeedTimeout,
	); err != nil {
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction seeder: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, names.InstructionSeeder); err != nil {
		return failf(CheckControlPlaneIsolation,
			"delete stopped vendor-instruction seeder: %v", err)
	}
	if err := b.verifyContainerAbsent(
		ctx,
		names.InstructionSeeder,
		st.instructionSeeder,
		st.ownershipLabel,
		CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.instructionSeeder = objectClaim{}
	return nil
}

func (b *Backend) copyInstructionInput(
	ctx context.Context, id, hostDir, targetDir string,
) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
	defer cancel()
	if err := b.rt.CopyIntoContainer(ctx, id, hostDir, targetDir); err != nil {
		return failf(CheckControlPlaneIsolation,
			"copy vendor-instruction input at %s: %v", targetDir, err)
	}
	return nil
}

// observeVendorInstructions proves the seeder's result from a different VM
// through a read-only mount. It checks both content identity and topology:
// present means exactly CLAUDE.md and no neighbor; absent means an empty
// overlay, which masks any image-baked user instruction directory.
func (b *Backend) observeVendorInstructions(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) error {
	spec := buildInstructionObserverSpec(b.cfg, hs, names, st.ownershipLabel)
	st.instructionObserver.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckControlPlaneIsolation,
			"create vendor-instruction observer: %v", err)
	}
	st.instructionObserver.owned = true
	rep, err := b.rt.Inspect(ctx, names.InstructionObserver)
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"inspect vendor-instruction observer before execution: %v", err)
	}
	if err := verifySeedRoleAllowlist(
		rep,
		spec,
		names.Instructions,
		instructionVolumeTarget,
		CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.instructionObserver.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction observer %q: %v", names.InstructionObserver, err)
	}
	if err := b.rt.StartContainer(ctx, names.InstructionObserver); err != nil {
		return failf(CheckControlPlaneIsolation,
			"start vendor-instruction observer: %v", err)
	}
	if err := b.waitStopped(
		ctx,
		names.InstructionObserver,
		st.instructionObserver,
		st.ownershipLabel,
		b.cfg.SeedTimeout,
	); err != nil {
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction observer: %v", err)
	}
	proof, err := b.readInstructionProof(ctx, hs.RunID, names.InstructionObserver, st)
	if err != nil {
		return err
	}
	if err := verifyInstructionProof(
		proof, st.ownershipLabel.Value, expectedInstructions(hs, st),
	); err != nil {
		return err
	}
	if st.preparedInstructions != nil && st.journalOpen {
		if err := b.cfg.Journal.MarkInstructionsPrepared(
			ctx, hs.RunID, *st.preparedInstructions,
		); err != nil {
			return failf(
				CheckControlPlaneIsolation,
				"journal prepared instructions: %v",
				err,
			)
		}
	}
	if err := b.rt.DeleteContainer(ctx, names.InstructionObserver); err != nil {
		return failf(CheckControlPlaneIsolation,
			"delete stopped vendor-instruction observer: %v", err)
	}
	if err := b.verifyContainerAbsent(
		ctx,
		names.InstructionObserver,
		st.instructionObserver,
		st.ownershipLabel,
		CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.instructionObserver = objectClaim{}
	return nil
}

func expectedInstructions(hs HandoffSpec, st *runState) VendorInstructions {
	if st.preparedInstructions == nil {
		return hs.Agent.VendorInstructions
	}
	return VendorInstructions{
		Vendor:   hs.Agent.VendorInstructions.Vendor,
		Delivery: hs.Agent.VendorInstructions.Delivery,
		Present:  true,
		Digest: domain.Digest(
			"sha256:" + st.preparedInstructions.BundleDigest,
		),
		Body: bytes.Clone(st.instructionBundleBody),
	}
}

const maxInstructionProofBytes = 4 << 10

func (b *Backend) readInstructionProof(
	ctx context.Context, runID, id string, st *runState,
) ([]byte, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+runID+"-instruction-proof-")
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation,
			"create vendor-instruction proof directory: %v", err)
	}
	st.instructionArchiveDir = dir
	defer func() {
		_ = os.RemoveAll(dir)
		st.instructionArchiveDir = ""
	}()
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(
		ctx, id, tarPath, CheckControlPlaneIsolation,
	); err != nil {
		return nil, err
	}
	file, err := os.Open(tarPath) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation,
			"open vendor-instruction proof archive: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only temp file removed above
	data, found, err := extractArchiveRegularFile(
		file, instructionProofPath, maxInstructionProofBytes,
	)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation,
			"read vendor-instruction proof: %v", err)
	}
	if !found {
		return nil, failf(CheckControlPlaneIsolation,
			"vendor-instruction observer produced no proof")
	}
	return data, nil
}

func verifyInstructionProof(
	data []byte, nonce string, expected VendorInstructions,
) error {
	fields := make(map[string]string, 4)
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return failf(CheckControlPlaneIsolation,
				"vendor-instruction proof carries a line that is not key=value")
		}
		if _, duplicate := fields[key]; duplicate {
			return failf(CheckControlPlaneIsolation,
				"vendor-instruction proof repeats %q", key)
		}
		fields[key] = value
	}
	if len(fields) != 4 || fields["nonce"] != nonce || fields["contents"] != "clean" {
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction proof metadata does not match the admitted topology")
	}
	switch {
	case expected.Present:
		if fields["present"] != "yes" ||
			!contentaddr.Valid("sha256:"+fields["digest"]) ||
			"sha256:"+fields["digest"] != string(expected.Digest) {
			return failf(CheckControlPlaneIsolation,
				"vendor-instruction proof does not match the admitted digest")
		}
	case fields["present"] != "no" || fields["digest"] != "none":
		return failf(CheckControlPlaneIsolation,
			"vendor-instruction proof does not show the admitted absence")
	}
	return nil
}
