package ward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// codexReviewInputRoles names a review's input volume and the seeder and
// observer that fill and prove it. They run the writer handoff's input
// scripts (buildInputSeederSpec, buildInputObserverSpec) on the review's
// pinned observer image, so both paths accept the same proof.
func codexReviewInputRoles(cfg CodexReviewConfig, runID string) inputVolumeRoles {
	names := codexReviewNames(runID)
	return inputVolumeRoles{
		runID: runID, volume: names.inputsVolume, seeder: names.inputsSeeder, observer: names.inputsObserver,
		image: cfg.ObserverImage, size: DefaultLaunchSize(LaunchReview),
	}
}

// codexReviewInputsStagePath is the run-keyed private stage under the trusted
// ExportRoot, so recovery re-derives it from the same config and wipes what a
// crash left behind.
func codexReviewInputsStagePath(exportRoot, runID string) string {
	return filepath.Join(exportRoot, ".codex-review-inputs-stage-"+runID)
}

// prepareCodexReviewInputs creates, seeds, and proves the review's input
// volume before the review container can exist. claim lets the caller's
// deferred cleanup delete the volume when a later step fails.
func (b *CodexReviewLifecycle) prepareCodexReviewInputs(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	roles inputVolumeRoles, owner Label, claim *objectClaim,
) (CodexReviewInputsObservation, error) {
	claim.attempted = true
	if err := b.rt.CreateVolume(
		ctx, roles.volume, inputVolumeSizeMB(launch.Inputs), append(runLabels(launch.RunID), owner),
	); err != nil {
		return CodexReviewInputsObservation{}, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "create Codex review input volume: %v", err)
	}
	claim.owned = true
	report, err := b.rt.InspectVolume(ctx, roles.volume)
	if err != nil {
		return CodexReviewInputsObservation{}, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "inspect Codex review input volume: %v", err)
	}
	claim.fingerprint, err = ownedFingerprint(report.CreationDate, report.Labels, report.LabelsObserved, owner)
	if err != nil {
		return CodexReviewInputsObservation{}, failw(
			CheckControlPlaneIsolation, err, "authenticate Codex review input volume: %v", err)
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: roles.volume, OwnershipToken: owner.Value, Fingerprint: claim.fingerprint}); err != nil {
		return CodexReviewInputsObservation{}, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal Codex review input volume: %v", err)
	}
	if err := b.seedCodexReviewInputs(ctx, cfg, launch, roles, owner); err != nil {
		return CodexReviewInputsObservation{}, err
	}
	return b.observeCodexReviewInputs(ctx, cfg, roles, launch.Inputs, owner, report)
}

// seedCodexReviewInputs writes the admitted files into one archive from the
// trusted blob store and extracts it onto the volume in a networkless seeder
// that is proven absent afterward. A blob that does not match its admitted
// digest or size is a contradiction; other staging failures are transient.
func (b *CodexReviewLifecycle) seedCodexReviewInputs(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec, roles inputVolumeRoles, owner Label,
) (retErr error) {
	dir := codexReviewInputsStagePath(b.cfg.ExportRoot, launch.RunID)
	if err := createPrivateStageDir(dir); err != nil {
		return codexReviewOperationalCheckf(CheckControlPlaneIsolation, "create Codex review input stage: %v", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			retErr = errors.Join(retErr, codexReviewOperationalCheckf(
				CheckControlPlaneIsolation, "wipe Codex review input stage: %v", rmErr))
		}
	}()
	input, ready := filepath.Join(dir, "input"), filepath.Join(dir, "ready")
	for _, sub := range []string{input, ready} {
		if err := os.Mkdir(sub, 0o700); err != nil {
			return codexReviewOperationalCheckf(CheckControlPlaneIsolation, "create Codex review input stage: %v", err)
		}
	}
	if err := writeInputArchive(
		ctx, filepath.Join(input, inputArchiveName), launch.Inputs, b.cfg.InputBlobs,
	); errors.Is(err, ErrInputBlobMismatch) {
		return failw(CheckControlPlaneIsolation, err, "stage Codex review inputs: %v", err)
	} else if err != nil {
		return codexReviewOperationalCheckf(CheckControlPlaneIsolation, "stage Codex review inputs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return codexReviewOperationalCheckf(CheckControlPlaneIsolation, "write Codex review input sentinel: %v", err)
	}
	spec := buildInputSeederSpec(b.cfg.seedConfig(), roles, owner)
	claim := objectClaim{attempted: true}
	defer func() {
		if retErr == nil || !claim.attempted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
		defer cancel()
		if cleanupErr := b.reapUnlistedContainer(cleanupCtx, spec.Name, claim, owner); cleanupErr != nil {
			retErr = errors.Join(retErr, failw(
				CheckControlPlaneIsolation, cleanupErr, "Codex review input seeder cleanup: %v", cleanupErr))
		}
	}()
	return b.runHelperRole(ctx, helperRole{
		spec: spec, label: "Codex review input seeder", claim: &claim, owner: owner,
		check: CheckControlPlaneIsolation, class: LaunchReview, timeout: b.cfg.SeedTimeout,
		verify: func(rep InspectReport) error {
			return verifySeedRoleAllowlist(rep, spec, roles.volume, inputVolumeTarget, CheckControlPlaneIsolation)
		},
		journal: func(fingerprint string) error {
			if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID, CodexReviewIntentResource{
				Name: spec.Name, OwnershipToken: owner.Value, Fingerprint: fingerprint,
			}); err != nil {
				return codexReviewJournalCheckf(CheckControlPlaneIsolation, "journal Codex review input seeder: %v", err)
			}
			return nil
		},
		afterStart: func(ctx context.Context) error {
			for _, copy := range []struct{ source, target string }{{input, inputStageDir}, {ready, inputReadyDir}} {
				if err := b.copyIntoSeeder(ctx, spec.Name, copy.source, copy.target); err != nil {
					return codexReviewOperationalCheckf(CheckControlPlaneIsolation, "copy Codex review inputs into seeder: %v", err)
				}
			}
			return nil
		},
		wrapRuntimeFailure: codexReviewOperational,
	})
}

// observeCodexReviewInputs runs a fresh read-only observer over the input
// volume and accepts it only when its nonce-bound listing digest matches the
// admitted files. Every call mints a new observer owner, so the pre-start
// re-observation is distinguishable from the first.
func (b *CodexReviewLifecycle) observeCodexReviewInputs(
	ctx context.Context, cfg CodexReviewConfig, roles inputVolumeRoles, files []InputFile,
	volumeOwner Label, volumeReport VolumeSummary,
) (CodexReviewInputsObservation, error) {
	if volumeReport.Name != roles.volume {
		return CodexReviewInputsObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review input observation identified the wrong volume")
	}
	fingerprint, err := ownedFingerprint(
		volumeReport.CreationDate, volumeReport.Labels, volumeReport.LabelsObserved, volumeOwner,
	)
	if err != nil {
		return CodexReviewInputsObservation{}, failw(
			CheckControlPlaneIsolation, err, "Codex review input volume ownership: %v", err)
	}
	observerOwner, err := newOwnershipLabel()
	if err != nil {
		return CodexReviewInputsObservation{}, failw(CheckControlPlaneIsolation, err,
			"mint Codex review input observer ownership: %v", err)
	}
	spec := buildInputObserverSpec(roles, observerOwner)
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, roles.runID,
		CodexReviewIntentResource{Name: spec.Name, OwnershipToken: observerOwner.Value}); err != nil {
		return CodexReviewInputsObservation{}, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal input observer intent: %v", err)
	}
	report, proof, err := b.runCodexReviewObserver(
		ctx, spec, observerOwner, stateProofPath, CheckControlPlaneIsolation,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = observerOwner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, roles.runID, resource)
		},
		func(rep InspectReport) error {
			return verifySeedRoleAllowlist(rep, spec, roles.volume, inputVolumeTarget, CheckControlPlaneIsolation)
		},
	)
	if err != nil {
		return CodexReviewInputsObservation{}, err
	}
	if err := verifyInputProof(proof, observerOwner.Value, files); err != nil {
		return CodexReviewInputsObservation{}, err
	}
	observerCreation, err := ownedFingerprint(report.CreationDate, report.Labels, report.LabelsObserved, observerOwner)
	if err != nil {
		return CodexReviewInputsObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review input observer ownership is not fingerprinted")
	}
	return CodexReviewInputsObservation{
		volume: roles.volume, fingerprint: fingerprint, digest: inputListingDigest(files),
		observerImage:       cfg.ObserverImage,
		observerFingerprint: codexReviewObserverFingerprint(observerCreation, observerOwner),
	}, nil
}
