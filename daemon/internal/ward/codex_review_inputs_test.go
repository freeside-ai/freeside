package ward

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// testReviewInputsRequest adds two operator files and their observed volume
// to a review request, as the lifecycle would after proving the volume.
func testReviewInputsRequest(cfg CodexReviewConfig, req CodexReviewSpec) CodexReviewSpec {
	files, _ := testInputs("task/shot.png", "pixels", "task/docs/notes.md", "notes")
	req.Inputs = files
	req.InputVolume = CodexReviewInputsObservation{
		volume: codexReviewInputsVolumeName(req.RunID), fingerprint: "inputs-created",
		digest: inputListingDigest(files), observerImage: cfg.ObserverImage,
		observerFingerprint: "inputs-observer-1",
	}
	return req
}

func inputMountsOf(spec ContainerSpec) []Mount {
	var mounts []Mount
	for _, m := range spec.Mounts {
		if m.Target == InputFilesTarget {
			mounts = append(mounts, m)
		}
	}
	return mounts
}

func TestCodexReviewSpecMountsObservedInputsReadOnly(t *testing.T) {
	t.Parallel()
	cfg, req := testCodexReview(t)
	req = testReviewInputsRequest(cfg, req)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	mounts := inputMountsOf(spec)
	if len(mounts) != 1 || !mounts[0].ReadOnly || mounts[0].Type != MountVolume ||
		mounts[0].Source != req.InputVolume.volume || spec.Mounts[len(spec.Mounts)-1] != mounts[0] {
		t.Fatalf("input mounts = %+v in %+v, want one read-only input volume last", mounts, spec.Mounts)
	}
	if binding.InputsVolume != req.InputVolume.volume || binding.InputsTarget != InputFilesTarget ||
		binding.InputsDigest != req.InputVolume.digest || binding.InputsFingerprint != req.InputVolume.fingerprint ||
		binding.InputsObserverImage != cfg.ObserverImage || !binding.InputsReadOnly ||
		binding.InputsPreStartObserverFingerprint != "" {
		t.Fatalf("binding input evidence = %+v", binding)
	}
	if err := binding.validatePrepared(codexReviewProvider{}); err != nil {
		t.Fatalf("prepared binding with inputs rejected: %v", err)
	}
	if err := validateReviewAgentSpec(codexReviewProvider{}, cfg, req, spec, binding); err != nil {
		t.Fatalf("conforming input spec rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ContainerSpec){
		"writable": func(s *ContainerSpec) { s.Mounts[len(s.Mounts)-1].ReadOnly = false },
		"retargeted source": func(s *ContainerSpec) {
			s.Mounts[len(s.Mounts)-1].Source = codexReviewSnapshotVolumeName(req.RunID)
		},
		"dropped": func(s *ContainerSpec) { s.Mounts = s.Mounts[:len(s.Mounts)-1] },
		"duplicated": func(s *ContainerSpec) {
			extra := s.Mounts[len(s.Mounts)-1]
			extra.Target = InputFilesTarget + "-2"
			s.Mounts = append(s.Mounts, extra)
		},
	} {
		changed := cloneContainerSpec(spec)
		mutate(&changed)
		if err := validateReviewAgentSpec(codexReviewProvider{}, cfg, req, changed, binding); err == nil {
			t.Errorf("%s input mount accepted", name)
		}
	}
	for name, mutate := range map[string]func(*CodexReviewJournalBinding){
		"writable":          func(b *CodexReviewJournalBinding) { b.InputsReadOnly = false },
		"partial":           func(b *CodexReviewJournalBinding) { b.InputsDigest = "" },
		"foreign volume":    func(b *CodexReviewJournalBinding) { b.InputsVolume = "freeside-review-other-inputs" },
		"retargeted":        func(b *CodexReviewJournalBinding) { b.InputsTarget = "/workspace/project/inputs" },
		"unpinned observer": func(b *CodexReviewJournalBinding) { b.InputsObserverImage = "exporter:latest" },
		"other digest":      func(b *CodexReviewJournalBinding) { b.InputsDigest = strings.Repeat("0", 64) },
	} {
		changed := cloneCodexReviewBinding(binding)
		mutate(&changed)
		if changed.validatePrepared(codexReviewProvider{}) == nil &&
			validateReviewAgentSpec(codexReviewProvider{}, cfg, req, spec, changed) == nil {
			t.Errorf("%s input binding accepted", name)
		}
	}
	dropped := cloneCodexReviewBinding(binding)
	dropped.InputsVolume, dropped.InputsTarget, dropped.InputsFingerprint, dropped.InputsDigest = "", "", "", ""
	dropped.InputsObserverImage, dropped.InputsObserverFingerprint, dropped.InputsReadOnly = "", "", false
	if err := validateReviewAgentSpec(codexReviewProvider{}, cfg, req, spec, dropped); err == nil {
		t.Error("binding without input evidence accepted for a request with inputs")
	}
}

func TestCodexReviewSpecRefusesUnprovenInputs(t *testing.T) {
	t.Parallel()
	cfg, base := testCodexReview(t)
	for name, mutate := range map[string]func(*CodexReviewSpec){
		"observation without files": func(r *CodexReviewSpec) { r.Inputs = nil },
		"files without observation": func(r *CodexReviewSpec) { r.InputVolume = CodexReviewInputsObservation{} },
		"other listing":             func(r *CodexReviewSpec) { r.InputVolume.digest = strings.Repeat("0", 64) },
		"other observer image": func(r *CodexReviewSpec) {
			r.InputVolume.observerImage = "example.test/other@sha256:" + strings.Repeat("d", 64)
		},
		"foreign volume": func(r *CodexReviewSpec) { r.InputVolume.volume = codexReviewInputsVolumeName("review-2") },
		"invalid name":   func(r *CodexReviewSpec) { r.Inputs[0].Name = "../escape.png" },
	} {
		req := testReviewInputsRequest(cfg, base)
		req.Inputs = slices.Clone(req.Inputs)
		mutate(&req)
		if _, _, err := BuildCodexReviewAgentSpec(cfg, req); err == nil {
			t.Errorf("%s: review spec built", name)
		}
	}
}

func TestCodexReviewInputObservationMustBeFreshBeforeStart(t *testing.T) {
	t.Parallel()
	cfg, req := testCodexReview(t)
	withInputs := testReviewInputsRequest(cfg, req)
	fresh := withInputs.InputVolume
	fresh.observerFingerprint = "inputs-observer-2"
	if err := withInputs.InputVolume.verifyFresh(fresh); err != nil {
		t.Fatalf("re-observed input volume rejected: %v", err)
	}
	for name, mutate := range map[string]func(*CodexReviewInputsObservation){
		"not re-observed": func(o *CodexReviewInputsObservation) {
			o.observerFingerprint = withInputs.InputVolume.observerFingerprint
		},
		"replaced volume": func(o *CodexReviewInputsObservation) { o.fingerprint = "replacement" },
		"changed files":   func(o *CodexReviewInputsObservation) { o.digest = strings.Repeat("0", 64) },
		"missing":         func(o *CodexReviewInputsObservation) { *o = CodexReviewInputsObservation{} },
	} {
		changed := fresh
		mutate(&changed)
		if err := withInputs.InputVolume.verifyFresh(changed); !errors.Is(err, ErrConformance) {
			t.Errorf("%s: fresh input observation = %v, want conformance failure", name, err)
		}
	}
}

func TestReviewLaunchDeliversInputsToEachProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider reviewProvider
		fixture  func(*testing.T) (CodexReviewConfig, CodexReviewSpec)
	}{
		{"codex", codexReviewProvider{}, testCodexReview},
		{"claude", claudeReviewProvider{}, testClaudeReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, req := tc.fixture(t)
			lifecycle, rt, cfg, launchSpec, journal := testReviewLifecycle(t, tc.provider, cfg, req)
			files, blobs := testInputs("task/A.png", "upper", "task/a.png", "lower", "task/docs/notes.md", "nested")
			lifecycle.cfg.InputBlobs = blobs
			launchSpec.Inputs = files
			names := reviewNames(tc.provider, launchSpec.RunID)
			created := false
			rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name != names.reviewContainer {
					return nil
				}
				created = true
				listing := rt.inputListing[names.inputsVolume]
				for _, f := range files {
					sum := sha256.Sum256(blobs[f.Digest])
					if !slices.Contains(listing, fmt.Sprintf("f %x ./%s", sum, f.Name)) {
						t.Errorf("%q did not arrive intact before the reviewer: %q", f.Name, listing)
					}
				}
				mounts := inputMountsOf(spec)
				if len(mounts) != 1 || !mounts[0].ReadOnly || mounts[0].Source != names.inputsVolume {
					t.Errorf("reviewer input mounts = %+v", mounts)
				}
				for _, helper := range []string{names.inputsSeeder, names.inputsObserver} {
					if _, exists := rt.ctrs[helper]; exists {
						t.Errorf("input helper %q still exists when the reviewer is created", helper)
					}
				}
				return nil
			}
			launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = launch.Close() })
			if !created {
				t.Fatal("reviewer never created")
			}
			b := launch.Binding
			if b.InputsVolume != names.inputsVolume || b.InputsDigest != inputListingDigest(files) ||
				!b.InputsReadOnly || b.InputsObserverFingerprint == "" ||
				b.InputsPreStartObserverFingerprint == "" ||
				b.InputsPreStartObserverFingerprint == b.InputsObserverFingerprint {
				t.Fatalf("launch binding input evidence = %+v", b)
			}
			if journal.intent.InputsVolume != names.inputsVolume {
				t.Fatalf("intent input volume = %q", journal.intent.InputsVolume)
			}
			stage := codexReviewInputsStagePath(lifecycle.cfg.ExportRoot, launchSpec.RunID)
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("input stage survived launch: %v", err)
			}
		})
	}
}

func TestReviewLaunchWithoutInputsCreatesNoInputVolume(t *testing.T) {
	t.Parallel()
	lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	names := codexReviewNames(launchSpec.RunID)
	for _, name := range []string{names.inputsVolume, names.inputsSeeder, names.inputsObserver} {
		if rt.callIndex("create-volume "+name) >= 0 || rt.callIndex("create-container "+name) >= 0 {
			t.Errorf("review without inputs created %q", name)
		}
	}
	if launch.Binding.InputsVolume != "" || journal.intent.InputsVolume != "" || len(inputMountsOf(rt.ctrs[names.reviewContainer].spec)) != 0 {
		t.Fatal("review without inputs carries input topology")
	}
}

func TestReviewInputPreparationFailuresNeverCreateReviewer(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{
		"no_blob_source", "blob_bytes", "blob_missing", "tampered_tree", "extra_entry", "wrong_proof",
		"replaced_before_start", "changed_before_start",
	} {
		t.Run(failure, func(t *testing.T) {
			lifecycle, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
			files, blobs := testInputs("task/shot.png", "pixels", "task/dir/notes.md", "notes")
			lifecycle.cfg.InputBlobs = blobs
			launchSpec.Inputs = files
			names := codexReviewNames(launchSpec.RunID)
			switch failure {
			case "no_blob_source":
				lifecycle.cfg.InputBlobs = nil
			case "blob_bytes":
				blobs[files[0].Digest] = []byte("pixelz")
			case "blob_missing":
				delete(blobs, files[0].Digest)
			}
			observers := 0
			rt.observerProof = func(id string, proof []byte) []byte {
				if failure == "wrong_proof" && id == names.inputsObserver {
					return []byte("foreign proof\n")
				}
				return proof
			}
			rt.onCreateContainer = func(spec ContainerSpec) error {
				switch spec.Name {
				case names.reviewContainer:
					// The pre-start re-observation runs against the created,
					// unstarted reviewer; every earlier failure stops first.
					if !strings.HasSuffix(failure, "_before_start") {
						t.Fatal("reviewer reached after failed input preparation")
					}
				case names.inputsObserver:
					observers++
					listing := rt.inputListing[names.inputsVolume]
					switch {
					case failure == "tampered_tree":
						shot := slices.IndexFunc(listing, func(l string) bool { return strings.HasSuffix(l, " ./task/shot.png") })
						listing[shot] = "f " + strings.Repeat("0", 64) + " ./task/shot.png"
					case failure == "extra_entry":
						rt.inputListing[names.inputsVolume] = append(listing, "x ./task/link")
					case failure == "changed_before_start" && observers == 2:
						rt.inputListing[names.inputsVolume] = append(listing, "f "+strings.Repeat("1", 64)+" ./task/late.png")
					case failure == "replaced_before_start" && observers == 2:
						rt.vols[names.inputsVolume].created = "replacement"
					}
				}
				return nil
			}
			launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
			if err == nil || launch != nil {
				t.Fatalf("unproven review inputs accepted: %v", err)
			}
			switch failure {
			case "no_blob_source":
				if !errors.Is(err, ErrInvalidCodexReviewSpec) {
					t.Fatalf("missing blob source = %v, want a configuration failure", err)
				}
			case "blob_bytes":
				var cf *ConformanceFailure
				if !errors.As(err, &cf) || !errors.Is(cf.Err, ErrInputBlobMismatch) {
					t.Fatalf("blob mismatch = %v, want a contradiction", err)
				}
			case "blob_missing":
				if !errors.Is(err, ErrCodexReviewOperational) {
					t.Fatalf("missing blob = %v, want a retryable failure", err)
				}
			default:
				if !errors.Is(err, ErrConformance) {
					t.Fatalf("unproven volume = %v, want a contradiction", err)
				}
			}
			if rt.callIndex("start-container "+names.reviewContainer) >= 0 {
				t.Fatal("reviewer started on unproven inputs")
			}
			for _, name := range []string{names.inputsSeeder, names.inputsObserver, names.reviewContainer} {
				if _, exists := rt.ctrs[name]; exists {
					t.Errorf("container %q survived the failed launch", name)
				}
			}
			// A replaced volume is no longer provably ward-owned, so cleanup
			// leaves it rather than delete what might be someone else's.
			_, survived := rt.vols[names.inputsVolume]
			if survived != (failure == "replaced_before_start") {
				t.Errorf("input volume survived the failed launch = %v", survived)
			}
			stage := codexReviewInputsStagePath(lifecycle.cfg.ExportRoot, launchSpec.RunID)
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("input stage survived the failed launch: %v", err)
			}
		})
	}
}

func TestReviewRecoveryReapsInterruptedInputPreparation(t *testing.T) {
	t.Parallel()
	lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	files, blobs := testInputs("task/shot.png", "pixels")
	lifecycle.cfg.InputBlobs = blobs
	launchSpec.Inputs = files
	names := codexReviewNames(launchSpec.RunID)
	// Fail at the network, after the input volume is proven, with cleanup
	// blocked: the process "dies" holding the whole input topology.
	blocked := errors.New("simulated process loss before cleanup")
	lost := false
	rt.onCreateNetwork = func(string) error { lost = true; return blocked }
	blockAfterLoss := func(string) (bool, error) {
		if lost {
			return true, blocked
		}
		return false, nil
	}
	rt.onDeleteContainer, rt.onDeleteVolume = blockAfterLoss, blockAfterLoss
	if _, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec); err == nil {
		t.Fatal("blocked launch succeeded")
	}
	if _, exists := rt.vols[names.inputsVolume]; !exists {
		t.Fatal("fixture did not leave the input volume behind")
	}
	stage := codexReviewInputsStagePath(lifecycle.cfg.ExportRoot, launchSpec.RunID)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	rt.onCreateNetwork, rt.onDeleteContainer, rt.onDeleteVolume = nil, nil, nil
	if journal.intent == nil || journal.intent.InputsVolume != names.inputsVolume {
		t.Fatalf("interrupted intent = %#v, want the input volume journaled", journal.intent)
	}
	workspace, workspaceVolume := journal.workspaceBinding, *rt.vols[launchSpec.WorkspaceVolume]
	if err := lifecycle.recoverCodexReviewIntent(t.Context(), cfg, *journal.intent, true); err != nil {
		t.Fatalf("recover interrupted input preparation: %v", err)
	}
	for _, name := range []string{names.inputsSeeder, names.inputsObserver} {
		if _, exists := rt.ctrs[name]; exists {
			t.Errorf("container %q survived recovery", name)
		}
	}
	if _, exists := rt.vols[names.inputsVolume]; exists {
		t.Error("input volume survived recovery")
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input stage survived recovery: %v", err)
	}
	// The restarted invocation retries the same launch and delivers the same
	// files again from the blob store. Recovery reaped the workspace, which
	// the source prepares again before it relaunches.
	journal.workspaceBinding = workspace
	rt.vols[launchSpec.WorkspaceVolume] = &workspaceVolume
	launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatalf("relaunch after recovery: %v", err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if launch.Binding.InputsDigest != inputListingDigest(files) ||
		len(rt.inputListing[names.inputsVolume]) == 0 {
		t.Fatalf("relaunch delivered %q under %+v", rt.inputListing[names.inputsVolume], launch.Binding)
	}
}

func TestReviewReconstructionRefusesADivergentInputBinding(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*CodexReviewJournalBinding){
		"other listing": func(b *CodexReviewJournalBinding) { b.InputsDigest = strings.Repeat("0", 64) },
		"other volume":  func(b *CodexReviewJournalBinding) { b.InputsFingerprint = "replacement" },
		"dropped evidence": func(b *CodexReviewJournalBinding) {
			b.InputsVolume, b.InputsTarget, b.InputsFingerprint, b.InputsDigest = "", "", "", ""
			b.InputsObserverImage, b.InputsObserverFingerprint, b.InputsReadOnly = "", "", false
		},
	} {
		t.Run(name, func(t *testing.T) {
			lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
			files, blobs := testInputs("task/shot.png", "pixels")
			lifecycle.cfg.InputBlobs = blobs
			launchSpec.Inputs = files
			journal.mutate = mutate
			if launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec); err == nil || launch != nil {
				t.Fatalf("divergent persisted input binding accepted: %v", err)
			}
			if rt.callIndex("start-container "+codexReviewContainerName(launchSpec.RunID)) >= 0 {
				t.Fatal("reviewer started on a divergent input binding")
			}
		})
	}
}

func TestReviewIntentInputResourcesMustMatchTheirNames(t *testing.T) {
	t.Parallel()
	runID := "review-1"
	names := codexReviewNames(runID)
	owner := testOwnershipLabel().Value
	intent := CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: "digest", OwnershipToken: owner,
		ShadowVolume: names.shadowVolume, Network: names.network, ReviewContainer: names.reviewContainer,
		SnapshotVolume: names.snapshotVolume, InputsVolume: names.inputsVolume, State: CodexReviewIntentPreparing,
		Resources: []CodexReviewIntentResource{
			{Name: names.workspaceObserver},
			{Name: names.shadowInitializer, OwnershipToken: owner},
			{Name: names.shadowObserver},
			{Name: names.reviewContainer, OwnershipToken: owner},
			{Name: names.shadowVolume, OwnershipToken: owner},
			{Name: names.network, OwnershipToken: owner},
			{Name: names.snapshotVolume, OwnershipToken: owner},
			{Name: names.snapshotSeeder, OwnershipToken: owner},
			{Name: names.snapshotObserver},
			{Name: names.inputsVolume, OwnershipToken: owner},
			{Name: names.inputsSeeder, OwnershipToken: owner},
			{Name: names.inputsObserver},
		},
	}
	got, err := intent.validatedResourceNames(runID)
	if err != nil || got.inputsVolume != names.inputsVolume {
		t.Fatalf("intent with inputs = %+v, %v", got, err)
	}
	withoutInputs := intent
	withoutInputs.InputsVolume = ""
	withoutInputs.Resources = slices.Clone(intent.Resources[:9])
	if got, err := withoutInputs.validatedResourceNames(runID); err != nil || got.inputsVolume != "" {
		t.Fatalf("intent without inputs = %+v, %v", got, err)
	}
	for name, mutate := range map[string]func(*CodexReviewLaunchIntent){
		"foreign volume": func(i *CodexReviewLaunchIntent) { i.InputsVolume = codexReviewInputsVolumeName("review-2") },
		"volume not journaled": func(i *CodexReviewLaunchIntent) {
			i.Resources = slices.Clone(i.Resources[:11])
		},
		"resources without volume": func(i *CodexReviewLaunchIntent) { i.InputsVolume = "" },
		"unowned seeder": func(i *CodexReviewLaunchIntent) {
			i.Resources = slices.Clone(i.Resources)
			i.Resources[10].OwnershipToken = ""
		},
	} {
		changed := intent
		mutate(&changed)
		if _, err := changed.validatedResourceNames(runID); err == nil {
			t.Errorf("%s intent accepted", name)
		}
	}
}

// TestReviewTeardownReapsTheInputVolume: a review's teardown deletes the
// input volume its intent journals, and refuses, leaving the volume, when the
// durable binding no longer names that volume.
func TestReviewTeardownReapsTheInputVolume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tamper bool
	}{{name: "reaped"}, {name: "binding_dropped_inputs", tamper: true}} {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
			files, blobs := testInputs("task/shot.png", "pixels")
			lifecycle.cfg.InputBlobs = blobs
			launchSpec.Inputs = files
			names := codexReviewNames(launchSpec.RunID)
			launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
			if err != nil {
				t.Fatal(err)
			}
			_ = launch.Close()
			if tc.tamper {
				journal.binding.InputsVolume = ""
			}
			err = lifecycle.AbortCodexReview(t.Context(), cfg, launchSpec.RunID)
			_, survived := rt.vols[names.inputsVolume]
			if tc.tamper {
				if !errors.Is(err, ErrConformance) || !survived {
					t.Fatalf("teardown under a binding without inputs = %v, volume survived=%v", err, survived)
				}
				return
			}
			if err != nil || survived {
				t.Fatalf("teardown = %v, input volume survived=%v", err, survived)
			}
		})
	}
}
