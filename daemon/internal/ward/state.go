package ward

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	configRootVolumeSizeMB int64 = 2
	continuityVolumeSizeMB int64 = 4096
	scratchVolumeSizeMB    int64 = 64
	maxStateProofBytes           = 4 << 10
)

type stateManifestKind string

const (
	stateManifestConfigRoot stateManifestKind = "config_root"
	stateManifestEmpty      stateManifestKind = "empty"
)

func (b *Backend) prepareLaunchState(
	ctx context.Context,
	hs HandoffSpec,
	names handoffNames,
	st *runState,
) error {
	if hs.Agent.LaunchState == LaunchStateNone {
		return nil
	}
	for _, volume := range []struct {
		name   string
		sizeMB int64
		claim  *objectClaim
	}{
		{names.ConfigRoot, configRootVolumeSizeMB, &st.configRoot},
		{names.Continuity, continuityVolumeSizeMB, &st.continuity},
		{names.SessionScratch, scratchVolumeSizeMB, &st.sessionScratch},
	} {
		if err := b.createStateVolume(ctx, hs.RunID, volume.name, volume.sizeMB, volume.claim, st); err != nil {
			return err
		}
	}
	for _, volume := range []struct {
		name   string
		target string
		kind   stateManifestKind
	}{
		{names.ConfigRoot, claudeConfigRootVolumeTarget, stateManifestConfigRoot},
		{names.Continuity, ClaudeContinuityTarget, stateManifestEmpty},
		{names.SessionScratch, ClaudeSessionScratchTarget, stateManifestEmpty},
	} {
		if err := b.seedLaunchStateVolume(
			ctx, hs.RunID, names.ConfigRootSeeder,
			volume.name, volume.target, volume.kind, st,
		); err != nil {
			return err
		}
	}
	configDigest, err := b.observeStateVolume(
		ctx, hs.RunID, names.ConfigRootObserver, names.ConfigRoot,
		stateManifestConfigRoot, &st.configRootObserver, st,
	)
	if err != nil {
		return err
	}
	continuityDigest, err := b.observeStateVolume(
		ctx, hs.RunID, names.ContinuityObserver, names.Continuity,
		stateManifestEmpty, &st.continuityObserver, st,
	)
	if err != nil {
		return err
	}
	scratchDigest, err := b.observeStateVolume(
		ctx, hs.RunID, names.ScratchObserver, names.SessionScratch,
		stateManifestEmpty, &st.scratchObserver, st,
	)
	if err != nil {
		return err
	}
	prepared := HandoffJournalState{
		ConfigRootFingerprint:     st.configRoot.fingerprint,
		ContinuityFingerprint:     st.continuity.fingerprint,
		SessionScratchFingerprint: st.sessionScratch.fingerprint,
		ConfigRootTarget:          ClaudeConfigRootTarget,
		ContinuityTarget:          ClaudeContinuityTarget,
		SessionScratchTarget:      ClaudeSessionScratchTarget,
		ConfigRootReadOnly:        true,
		ConfigRootDigest:          configDigest,
		ContinuityDigest:          continuityDigest,
		SessionScratchDigest:      scratchDigest,
	}
	st.preparedState = &prepared
	if st.journalOpen {
		if err := b.cfg.Journal.MarkStatePrepared(ctx, hs.RunID, prepared); err != nil {
			return fmt.Errorf("journal prepared launch state: %w", err)
		}
	}
	return nil
}

func (b *Backend) createStateVolume(
	ctx context.Context,
	runID, name string,
	sizeMB int64,
	claim *objectClaim,
	st *runState,
) error {
	claim.attempted = true
	labels := append(runLabels(runID), st.ownershipLabel)
	if err := b.rt.CreateVolume(ctx, name, sizeMB, labels); err != nil {
		return failf(CheckControlPlaneIsolation, "create launch-state volume %q: %v", name, err)
	}
	claim.owned = true
	view, err := b.rt.InspectVolume(ctx, name)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "inspect launch-state volume %q: %v", name, err)
	}
	if view.Name != name {
		return failf(CheckControlPlaneIsolation, "launch-state volume inspection returned the wrong identity")
	}
	claim.fingerprint, err = ownedFingerprint(
		view.CreationDate, view.Labels, view.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "launch-state volume %q: %v", name, err)
	}
	return nil
}

func (b *Backend) seedLaunchStateVolume(
	ctx context.Context,
	runID string,
	seederName, volume, target string,
	kind stateManifestKind,
	st *runState,
) error {
	spec := ContainerSpec{
		Name:            seederName,
		Image:           b.cfg.ExporterImage,
		Command:         []string{"sh", "-c", stateSeederScript(target, kind)},
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: target,
		}},
		Labels: append(runLabels(runID), st.ownershipLabel),
	}
	st.configRootSeeder.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return failf(CheckControlPlaneIsolation, "create launch-state seeder: %v", err)
	}
	st.configRootSeeder.owned = true
	rep, err := b.rt.Inspect(ctx, seederName)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "inspect launch-state seeder: %v", err)
	}
	if err := verifySeedRoleAllowlist(
		rep, spec, volume, target,
		CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.configRootSeeder.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "launch-state seeder ownership: %v", err)
	}
	if err := b.rt.StartContainer(ctx, seederName); err != nil {
		return failf(CheckControlPlaneIsolation, "start launch-state seeder: %v", err)
	}
	if err := b.waitStopped(
		ctx, seederName, st.configRootSeeder,
		st.ownershipLabel, b.cfg.SeedTimeout,
	); err != nil {
		return failf(CheckControlPlaneIsolation, "launch-state seeder: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, seederName); err != nil {
		return failf(CheckControlPlaneIsolation, "delete launch-state seeder: %v", err)
	}
	if err := b.verifyContainerAbsent(
		ctx, seederName, st.configRootSeeder,
		st.ownershipLabel, CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	st.configRootSeeder = objectClaim{}
	return nil
}

func stateSeederScript(target string, kind stateManifestKind) string {
	root := shellQuote(target)
	lostFound := shellQuote(target + "/lost+found")
	script := "set -eu; " +
		"if [ -e " + lostFound + " ]; then " +
		"test -d " + lostFound + "; test ! -L " + lostFound + "; " +
		"test \"$(stat -c '%a:%u:%g' " + lostFound + ")\" = '700:0:0'; " +
		"test -z \"$(find " + lostFound + " -mindepth 1 -maxdepth 1 -print -quit)\"; " +
		"rmdir " + lostFound + "; fi; "
	if kind == stateManifestConfigRoot {
		script += "umask 022; mkdir -p " + root + "/projects " + root + "/session-env; " +
			"chown 0:0 " + root + " " + root + "/projects " + root + "/session-env; " +
			"chmod 0755 " + root + " " + root + "/projects " + root + "/session-env; "
	}
	return script + "sync"
}

func buildStateObserverSpec(
	cfg Config,
	runID, name, volume string,
	kind stateManifestKind,
	ownership Label,
) ContainerSpec {
	return ContainerSpec{
		Name: name, Image: cfg.ExporterImage,
		Command: []string{"sh", "-c", stateObserverScript(
			ownership.Value, kind,
		)},
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type: MountVolume, Source: volume,
			Target: stateObserverVolumeTarget, ReadOnly: true,
		}},
		Labels: append(runLabels(runID), ownership),
	}
}

func stateObserverScript(nonce string, kind stateManifestKind) string {
	target := shellQuote(stateObserverVolumeTarget)
	validity := "valid=invalid; "
	switch kind {
	case stateManifestConfigRoot:
		validity += "entries=\"$(cd " + target +
			" && find . ! -name . -print | sort)\"; " +
			"if [ \"$entries\" = './projects\n./session-env' ] && " +
			"[ -d " + target + "/projects ] && [ ! -L " + target + "/projects ] && " +
			"[ -d " + target + "/session-env ] && [ ! -L " + target + "/session-env ] && " +
			"[ \"$(stat -c '%a:%u:%g' " + target + ")\" = '755:0:0' ] && " +
			"[ \"$(stat -c '%a:%u:%g' " + target + "/projects)\" = '755:0:0' ] && " +
			"[ \"$(stat -c '%a:%u:%g' " + target + "/session-env)\" = '755:0:0' ]; " +
			"then valid=valid; fi; "
	case stateManifestEmpty:
		validity += "entries=\"$(cd " + target +
			" && find . ! -name . -print | sort)\"; " +
			"if [ -z \"$entries\" ] && [ \"$(stat -c '%u:%g' " +
			target + ")\" = '0:0' ]; then valid=valid; fi; "
	}
	return "set -u; LC_ALL=C; export LC_ALL; " + validity +
		"digest=\"$(cd " + target +
		" 2>/dev/null && find . -exec stat -c '%n|%F|%a|%u|%g|%s' {} \\; " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"printf 'nonce=%s\\nkind=%s\\ncontents=%s\\ndigest=%s\\n' " +
		shellQuote(nonce) + " " + shellQuote(string(kind)) +
		" \"$valid\" \"$digest\" > " + shellQuote(stateProofPath) + "; sync"
}

func (b *Backend) observeStateVolume(
	ctx context.Context,
	runID, name, volume string,
	kind stateManifestKind,
	claim *objectClaim,
	st *runState,
) (string, error) {
	spec := buildStateObserverSpec(
		b.cfg, runID, name, volume, kind, st.ownershipLabel,
	)
	claim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return "", failf(CheckControlPlaneIsolation, "create state observer: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, name)
	if err != nil {
		return "", failf(CheckControlPlaneIsolation, "inspect state observer: %v", err)
	}
	if err := verifySeedRoleAllowlist(
		rep, spec, volume, stateObserverVolumeTarget, CheckControlPlaneIsolation,
	); err != nil {
		return "", err
	}
	claim.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return "", failf(CheckControlPlaneIsolation, "state observer ownership: %v", err)
	}
	if err := b.rt.StartContainer(ctx, name); err != nil {
		return "", failf(CheckControlPlaneIsolation, "start state observer: %v", err)
	}
	if err := b.waitStopped(
		ctx, name, *claim, st.ownershipLabel, b.cfg.SeedTimeout,
	); err != nil {
		return "", failf(CheckControlPlaneIsolation, "state observer: %v", err)
	}
	proof, err := b.readStateProof(ctx, runID, name, st)
	if err != nil {
		return "", err
	}
	digest, err := verifyStateProof(proof, st.ownershipLabel.Value, kind)
	if err != nil {
		return "", err
	}
	if err := b.rt.DeleteContainer(ctx, name); err != nil {
		return "", failf(CheckControlPlaneIsolation, "delete state observer: %v", err)
	}
	if err := b.verifyContainerAbsent(
		ctx, name, *claim, st.ownershipLabel, CheckControlPlaneIsolation,
	); err != nil {
		return "", err
	}
	*claim = objectClaim{}
	return digest, nil
}

func (b *Backend) readStateProof(
	ctx context.Context,
	runID, id string,
	st *runState,
) ([]byte, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+runID+"-state-proof-")
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "create state proof directory: %v", err)
	}
	st.stateArchiveDir = dir
	defer func() {
		_ = os.RemoveAll(dir)
		st.stateArchiveDir = ""
	}()
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(
		ctx, id, tarPath, CheckControlPlaneIsolation,
	); err != nil {
		return nil, err
	}
	file, err := os.Open(tarPath) //nolint:gosec // gate-owned temp path
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "open state proof archive: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only gate-owned temp
	data, found, err := extractArchiveRegularFile(
		file, stateProofPath, maxStateProofBytes,
	)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "read state proof: %v", err)
	}
	if !found {
		return nil, failf(CheckControlPlaneIsolation, "state observer produced no proof")
	}
	return data, nil
}

func verifyStateProof(
	data []byte,
	nonce string,
	kind stateManifestKind,
) (string, error) {
	required := map[string]string{
		"nonce": nonce, "kind": string(kind), "contents": "valid",
	}
	seen := map[string]bool{}
	var digest string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || seen[key] {
			return "", failf(CheckControlPlaneIsolation, "state proof is malformed")
		}
		seen[key] = true
		if key == "digest" {
			if !sha256HexPattern.MatchString(value) {
				return "", failf(CheckControlPlaneIsolation, "state proof digest is malformed")
			}
			digest = value
			continue
		}
		want, ok := required[key]
		if !ok || value != want {
			return "", failf(CheckControlPlaneIsolation, "state proof reports an unexpected value")
		}
	}
	if err := sc.Err(); err != nil {
		return "", failf(CheckControlPlaneIsolation, "state proof is unreadable")
	}
	if len(seen) != 4 || digest == "" {
		return "", failf(CheckControlPlaneIsolation, "state proof omits a required key")
	}
	return digest, nil
}

func (b *Backend) verifyPreparedLaunchState(
	ctx context.Context,
	hs HandoffSpec,
	names handoffNames,
	st *runState,
) error {
	if hs.Agent.LaunchState == LaunchStateNone {
		return nil
	}
	if st.preparedState == nil {
		return failf(CheckControlPlaneIsolation, "Claude launch state was not prepared")
	}
	if st.journalOpen {
		rec, err := b.cfg.Journal.Get(ctx, hs.RunID)
		if err != nil {
			return failf(
				CheckControlPlaneIsolation,
				"reload prepared launch bindings: %v",
				err,
			)
		}
		if rec.State == nil || *rec.State != *st.preparedState {
			return failf(CheckControlPlaneIsolation, "prepared state journal binding changed")
		}
		if st.preparedInstructions == nil || rec.Instructions == nil ||
			*rec.Instructions != *st.preparedInstructions {
			return failf(
				CheckControlPlaneIsolation,
				"prepared instruction journal binding changed",
			)
		}
	}
	for _, volume := range []struct {
		name        string
		fingerprint string
		claim       objectClaim
	}{
		{names.ConfigRoot, st.preparedState.ConfigRootFingerprint, st.configRoot},
		{names.Continuity, st.preparedState.ContinuityFingerprint, st.continuity},
		{names.SessionScratch, st.preparedState.SessionScratchFingerprint, st.sessionScratch},
	} {
		view, err := b.rt.InspectVolume(ctx, volume.name)
		if err != nil {
			return failf(CheckControlPlaneIsolation, "re-inspect state volume %q: %v", volume.name, err)
		}
		if view.Name != volume.name || volume.claim.fingerprint != volume.fingerprint {
			return failf(CheckControlPlaneIsolation, "state volume binding changed before launch")
		}
		switch classifyEvidence(
			volume.claim, st.ownershipLabel, view.CreationDate,
			view.Labels, view.LabelsObserved,
		) {
		case evidenceOurs:
		case evidenceForeign:
			return failf(CheckControlPlaneIsolation, "state volume was replaced before launch")
		case evidenceUnprovable:
			return failf(CheckControlPlaneIsolation, "state volume identity became unprovable")
		}
	}
	return nil
}
