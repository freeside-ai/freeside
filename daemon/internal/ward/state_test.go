package ward

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestClaudeLaunchStateTopologyIsPreparedAndJournalled(t *testing.T) {
	fx := newHandoffFixture(t)
	journal := newFakeJournal()
	fx.cfg.Journal = journal
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	names := namesFor(hs.RunID)
	var agent ContainerSpec
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == names.Agent {
			agent = cloneContainerSpec(spec)
		}
		return nil
	}

	result, err := fx.backend(t).Handoff(context.Background(), hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })

	want := map[string]Mount{
		ClaudeConfigRootTarget: {
			Type: MountVolume, Source: names.ConfigRoot,
			Target: ClaudeConfigRootTarget, ReadOnly: true,
		},
		ClaudeContinuityTarget: {
			Type: MountVolume, Source: names.Continuity,
			Target: ClaudeContinuityTarget,
		},
		ClaudeSessionScratchTarget: {
			Type: MountVolume, Source: names.SessionScratch,
			Target: ClaudeSessionScratchTarget,
		},
	}
	for _, mount := range agent.Mounts {
		if expected, ok := want[mount.Target]; ok {
			if mount != expected {
				t.Errorf("state mount at %s = %+v, want %+v", mount.Target, mount, expected)
			}
			delete(want, mount.Target)
		}
	}
	if len(want) != 0 {
		t.Errorf("agent omitted state mounts: %+v", want)
	}
	record := journal.snapshot(hs.RunID)
	if record == nil || record.State == nil || record.Instructions == nil {
		t.Fatal("closed journal carries no prepared state or instruction binding")
	}
	if record.State.ConfigRootFingerprint == "" ||
		record.State.ContinuityFingerprint == "" ||
		record.State.SessionScratchFingerprint == "" {
		t.Errorf("state binding carries an empty fingerprint: %+v", record.State)
	}
	fx.assertReaped(t)
}

func TestLaunchStateSeederNormalizesEveryFilesystemRoot(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	names := namesFor(hs.RunID)
	var seeded []ContainerSpec
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if spec.Name == names.ConfigRootSeeder {
			seeded = append(seeded, cloneContainerSpec(spec))
		}
		return nil
	}

	result, err := fx.backend(t).Handoff(context.Background(), hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })

	if len(seeded) != 3 {
		t.Fatalf("launch-state seeders = %d, want 3", len(seeded))
	}
	want := []struct {
		volume string
		target string
	}{
		{names.ConfigRoot, claudeConfigRootVolumeTarget},
		{names.Continuity, ClaudeContinuityTarget},
		{names.SessionScratch, ClaudeSessionScratchTarget},
	}
	for i, spec := range seeded {
		if len(spec.Mounts) != 1 ||
			spec.Mounts[0].Source != want[i].volume ||
			spec.Mounts[0].Target != want[i].target {
			t.Errorf("seeder %d mounts = %+v, want %s at %s",
				i, spec.Mounts, want[i].volume, want[i].target)
		}
		script := spec.Command[2]
		lostFound := shellQuote(want[i].target + "/lost+found")
		for _, required := range []string{
			"test -d " + lostFound,
			"test ! -L " + lostFound,
			"'700:0:0'",
			"-mindepth 1 -maxdepth 1 -print -quit",
			"rmdir " + lostFound,
		} {
			if !strings.Contains(script, required) {
				t.Errorf("seeder %d omits %q: %s", i, required, script)
			}
		}
		if strings.Contains(script, "rm -") {
			t.Errorf("seeder %d uses recursive or forced deletion: %s", i, script)
		}
	}
	fx.assertReaped(t)
}

func TestVerifyStateProofRejectsEveryUntrustedShape(t *testing.T) {
	const nonce = "00112233445566778899aabbccddeeff"
	digest := strings.Repeat("ab", 32)
	valid := "nonce=" + nonce + "\nkind=empty\ncontents=valid\ndigest=" + digest + "\n"
	if got, err := verifyStateProof(
		[]byte(valid), nonce, stateManifestEmpty,
	); err != nil || got != digest {
		t.Fatalf("verifyStateProof(valid) = %q, %v, want digest, nil", got, err)
	}
	for _, proof := range []string{
		"",
		"nonce=" + nonce + "\nkind=empty\ncontents=valid\n",
		valid + "extra=1\n",
		valid + "digest=" + digest + "\n",
		strings.Replace(valid, "contents=valid", "contents=invalid", 1),
		strings.Replace(valid, "kind=empty", "kind=config_root", 1),
		strings.Replace(valid, digest, strings.ToUpper(digest), 1),
	} {
		if _, err := verifyStateProof(
			[]byte(proof), nonce, stateManifestEmpty,
		); !errors.Is(err, ErrConformance) {
			t.Errorf("verifyStateProof(%q) = %v, want ErrConformance", proof, err)
		}
	}
}

func TestClaudeLaunchStateReplacementRefusesBeforeStart(t *testing.T) {
	fx := newHandoffFixture(t)
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	names := namesFor(hs.RunID)
	continuityInspects := 0
	fx.rt.onInspectVolume = func(name string, view VolumeSummary) (VolumeSummary, error) {
		if name == names.Continuity {
			continuityInspects++
			if continuityInspects == 2 {
				view.CreationDate = "replacement-volume"
			}
		}
		return view, nil
	}

	_, err := fx.backend(t).Handoff(context.Background(), hs)
	wantCheckFailure(t, err, CheckControlPlaneIsolation)
	if fx.rt.callIndex("start-container "+names.Agent) >= 0 {
		t.Fatal("writer started after a state-volume identity substitution")
	}
	fx.assertReaped(t)
}

func TestClaudeInstructionJournalReplacementRefusesBeforeStart(t *testing.T) {
	fx := newHandoffFixture(t)
	journal := newFakeJournal()
	fx.cfg.Journal = journal
	hs := fx.seed(t)
	hs.Agent.LaunchState = LaunchStateClaudeClean
	names := namesFor(hs.RunID)
	tampered := false
	journal.onCall = func(call string) {
		if tampered || call != "journal-get "+hs.RunID {
			return
		}
		rec := journal.snapshot(hs.RunID)
		if rec == nil || rec.Instructions == nil {
			t.Fatal("pre-start reload preceded instruction preparation")
		}
		rec.Instructions.BundleDigest = strings.Repeat("ff", 32)
		journal.put(*rec)
		tampered = true
	}

	_, err := fx.backend(t).Handoff(context.Background(), hs)
	wantCheckFailure(t, err, CheckControlPlaneIsolation)
	if !tampered {
		t.Fatal("handoff never reloaded its prepared instruction binding")
	}
	if fx.rt.callIndex("start-container "+names.Agent) >= 0 {
		t.Fatal("writer started after an instruction journal substitution")
	}
	fx.assertReaped(t)
}

func TestRecoverReapsJournalBoundClaudeStateVolumes(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	hs.Seed = WorkspaceSeed{
		Mode:      SeedBaseCheckout,
		SourceDir: "/trusted/base",
		Base:      testBaseRevision(),
	}
	hs.Agent.LaunchState = LaunchStateClaudeClean
	fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	labels := fx.runLabels(hs.RunID)
	ownership := Label{Key: ownershipLabelKey, Value: testRecoveryToken}
	state := HandoffJournalState{
		ConfigRootDigest:     strings.Repeat("ab", 32),
		ContinuityDigest:     strings.Repeat("cd", 32),
		SessionScratchDigest: strings.Repeat("ef", 32),
		ConfigRootTarget:     ClaudeConfigRootTarget,
		ContinuityTarget:     ClaudeContinuityTarget,
		SessionScratchTarget: ClaudeSessionScratchTarget,
		ConfigRootReadOnly:   true,
	}
	for _, volume := range []struct {
		name        string
		fingerprint *string
	}{
		{names.ConfigRoot, &state.ConfigRootFingerprint},
		{names.Continuity, &state.ContinuityFingerprint},
		{names.SessionScratch, &state.SessionScratchFingerprint},
	} {
		fx.worldVolume(t, volume.name, labels)
		view, err := fx.rt.InspectVolume(context.Background(), volume.name)
		if err != nil {
			t.Fatalf("InspectVolume(%s): %v", volume.name, err)
		}
		*volume.fingerprint, err = ownedFingerprint(
			view.CreationDate, view.Labels, view.LabelsObserved, ownership,
		)
		if err != nil {
			t.Fatalf("fingerprint %s: %v", volume.name, err)
		}
	}
	instructions := HandoffJournalInstructions{
		CompositionVersion:       instructionCompositionVersion,
		HostDigest:               instructionSourceAbsent,
		RepositoryManifestDigest: strings.Repeat("12", 32),
		BundleDigest:             strings.Repeat("34", 32),
	}
	if err := fx.j.MarkInstructionsPrepared(
		context.Background(), hs.RunID, instructions,
	); err != nil {
		t.Fatalf("MarkInstructionsPrepared: %v", err)
	}
	if err := fx.j.MarkStatePrepared(context.Background(), hs.RunID, state); err != nil {
		t.Fatalf("MarkStatePrepared: %v", err)
	}

	result, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != RecoveryLoss {
		t.Fatalf("outcome = %q, want loss", result.Outcome)
	}
	fx.assertReaped(t)
}
