package ward

import (
	"context"
	"os"
	"path/filepath"
)

// stagedVolume is one read-only writer input ward fills on a fresh state
// volume. A network-disabled seeder waits for host-staged files copied into
// its root filesystem and writes them onto the volume; a separate observer
// mounts the volume read-only and writes only a nonce-bound proof, which
// verify checks before the writer can exist.
type stagedVolume struct {
	label         string
	volume        string
	volumeTarget  string
	sizeMB        int64
	volumeClaim   *objectClaim
	seeder        ContainerSpec
	seederClaim   *objectClaim
	observer      ContainerSpec
	observerClaim *objectClaim
	stageDir      string
	readyDir      string
	// stage writes the seeder's input files into a fresh host directory.
	stage func(dir string) error
	// verify checks the observer's proof against the immutable input.
	verify func(proof []byte) error
}

// prepareStagedVolume uses the controlled rootfs-copy handshake. Runtime copy
// directly into a mounted volume does not reliably populate that volume.
func (b *Backend) prepareStagedVolume(ctx context.Context, hs HandoffSpec, sv stagedVolume, st *runState) error {
	if err := b.createStateVolume(ctx, hs.RunID, sv.volume, sv.sizeMB, sv.volumeClaim, st); err != nil {
		return err
	}
	snapshot, err := os.MkdirTemp("", "freeside-"+sv.label+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshot) //nolint:errcheck // gate-owned transient input
	input, ready := filepath.Join(snapshot, "input"), filepath.Join(snapshot, "ready")
	for _, dir := range []string{input, ready} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	if err := sv.stage(input); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return err
	}
	if err := b.startStagedRole(ctx, sv.seeder, sv.volume, sv.volumeTarget, sv.seederClaim, st); err != nil {
		return err
	}
	for _, copy := range []struct{ source, target string }{{input, sv.stageDir}, {ready, sv.readyDir}} {
		copyCtx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
		err := b.rt.CopyIntoContainer(copyCtx, sv.seeder.Name, copy.source, copy.target)
		cancel()
		if err != nil {
			return failw(CheckControlPlaneIsolation, err, "copy %s input: %v", sv.label, err)
		}
	}
	if err := b.waitStopped(ctx, sv.seeder.Name, *sv.seederClaim, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return err
	}
	if err := b.helperStopped(ctx, hs.Class, hs.Size, sv.seeder.Name); err != nil {
		return err
	}
	if err := b.removeStagedRole(ctx, sv.seeder.Name, sv.seederClaim, st); err != nil {
		return err
	}
	if err := b.startStagedRole(ctx, sv.observer, sv.volume, sv.volumeTarget, sv.observerClaim, st); err != nil {
		return err
	}
	if err := b.waitStopped(ctx, sv.observer.Name, *sv.observerClaim, st.ownershipLabel, b.cfg.SeedTimeout); err != nil {
		return err
	}
	if err := b.helperStopped(ctx, hs.Class, hs.Size, sv.observer.Name); err != nil {
		return err
	}
	// Reuse the bounded rootfs proof reader. The observer exports only its
	// nonce/digest proof; the content itself lives on the separate volume.
	proof, err := b.readStateProof(ctx, hs.RunID, sv.observer.Name, st)
	if err != nil {
		return err
	}
	if err := sv.verify(proof); err != nil {
		return err
	}
	return b.removeStagedRole(ctx, sv.observer.Name, sv.observerClaim, st)
}

func (b *Backend) startStagedRole(
	ctx context.Context, spec ContainerSpec, volume, target string, claim *objectClaim, st *runState,
) error {
	claim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failw(CheckControlPlaneIsolation, err, "create staged-volume role: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return err
	}
	if err := verifySeedRoleAllowlist(rep, spec, volume, target, CheckControlPlaneIsolation); err != nil {
		return err
	}
	claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return err
	}
	return b.rt.StartContainer(ctx, spec.Name)
}

func (b *Backend) removeStagedRole(ctx context.Context, name string, claim *objectClaim, st *runState) error {
	if err := b.rt.DeleteContainer(ctx, name); err != nil {
		return err
	}
	if err := b.verifyContainerAbsent(ctx, name, *claim, st.ownershipLabel, CheckControlPlaneIsolation); err != nil {
		return err
	}
	*claim = objectClaim{}
	return nil
}
