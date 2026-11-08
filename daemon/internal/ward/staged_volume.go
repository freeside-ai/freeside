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
// verify checks before the writer can exist. The prompt file and the operator
// input directory share this handshake.
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
	seeder := b.stagedRole(hs, sv, sv.seeder, sv.seederClaim, "seeder", st)
	seeder.afterStart = func(ctx context.Context) error {
		for _, copy := range []struct{ source, target string }{{input, sv.stageDir}, {ready, sv.readyDir}} {
			copyCtx, cancel := context.WithTimeout(ctx, b.cfg.SeedTimeout)
			err := b.rt.CopyIntoContainer(copyCtx, sv.seeder.Name, copy.source, copy.target)
			cancel()
			if err != nil {
				return failw(CheckControlPlaneIsolation, err, "copy %s input: %v", sv.label, err)
			}
		}
		return nil
	}
	if err := b.runtimeOps.runHelperRole(ctx, seeder); err != nil {
		return err
	}
	observer := b.stagedRole(hs, sv, sv.observer, sv.observerClaim, "observer", st)
	// Reuse the bounded rootfs proof reader. The observer exports only its
	// nonce/digest proof; the content itself lives on the separate volume.
	observer.readProof = func(ctx context.Context) ([]byte, error) {
		return b.readStateProof(ctx, hs.RunID, sv.observer.Name, st)
	}
	observer.checkProof = sv.verify
	return b.runtimeOps.runHelperRole(ctx, observer)
}

// stagedRole is the part of a staged volume's seeder or observer the two
// share: both mount the one volume and pass the seed-role allowlist.
func (b *Backend) stagedRole(
	hs HandoffSpec, sv stagedVolume, spec ContainerSpec, claim *objectClaim, role string, st *runState,
) helperRole {
	return helperRole{
		spec: spec, label: sv.label + " " + role, claim: claim, owner: st.ownershipLabel,
		check: CheckControlPlaneIsolation, class: hs.Class, timeout: b.cfg.SeedTimeout,
		verify: func(rep InspectReport) error {
			return verifySeedRoleAllowlist(rep, spec, sv.volume, sv.volumeTarget, CheckControlPlaneIsolation)
		},
	}
}
