package ward

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// seedVendorInstructions writes exactly the admitted host instruction file,
// or an empty directory for explicit absence, into the run-owned instruction
// volume. The seeder is pinned, credential-free, and network-free; an
// independent read-only observer below proves what actually landed.
func (b *Backend) seedVendorInstructions(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) error {
	snapshot, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-instructions-")
	if err != nil {
		return failf(CheckControlPlaneIsolation,
			"create vendor-instruction snapshot directory: %v", err)
	}
	st.instructionSnapshotDir = snapshot
	if hs.Agent.VendorInstructions.Present {
		if err := os.WriteFile(
			filepath.Join(snapshot, instructionFileName),
			hs.Agent.VendorInstructions.Body,
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
		proof, st.ownershipLabel.Value, hs.Agent.VendorInstructions,
	); err != nil {
		return err
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
