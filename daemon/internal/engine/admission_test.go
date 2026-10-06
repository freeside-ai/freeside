package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type replayInspectionDriver struct {
	inspection exec.Inspection
	err        error
}

func (d replayInspectionDriver) Start(context.Context, domain.InvocationID, exec.StartSpec) error {
	return nil
}

func (d replayInspectionDriver) Inspect(context.Context, domain.InvocationID) (exec.Inspection, error) {
	return d.inspection, d.err
}

func (d replayInspectionDriver) Stream(context.Context, domain.InvocationID) (io.ReadCloser, error) {
	return nil, exec.ErrUnknownInvocation
}

func (d replayInspectionDriver) Cancel(context.Context, domain.InvocationID) error { return nil }

func (d replayInspectionDriver) Collect(context.Context, domain.InvocationID) (exec.StageResult, error) {
	return exec.StageResult{}, exec.ErrResultNotReady
}

type stageInputBackend struct{}

func (stageInputBackend) Name() string { return "stage-input-test" }

func (stageInputBackend) Capabilities() exec.CapabilitySet {
	return exec.NewCapabilitySet(exec.CapPostExitExport)
}

func TestWaivedPostureItemIsExplicitlyBlocking(t *testing.T) {
	createdAt := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	item, err := waivedPostureItem(
		domain.Run{ID: "run-1", ProjectID: "proj-1"},
		"inv-1",
		domain.BackupEncryptionWaiver{RepositoryID: 42, Reason: "temporary operator waiver"},
		createdAt,
	)
	if err != nil {
		t.Fatalf("waivedPostureItem: %v", err)
	}
	if item.Posture == nil || *item.Posture != domain.HealthPostureBlocking {
		t.Fatalf("waived posture item = %v, want blocking", item.Posture)
	}
	if item.CreatedAt == nil || !item.CreatedAt.Equal(createdAt) {
		t.Fatalf("waived posture created_at = %v, want %v", item.CreatedAt, createdAt)
	}
}

func TestWithAdmissionRejectsNonCanonicalPromptDigest(t *testing.T) {
	option := WithAdmission(
		stageInputBackend{}, nil,
		AdmissionEnvironment{
			PromptPackageDigest: "sha256:not-hex",
			VendorInstructions: VendorInstructionConfig{
				Vendor:   domain.AgentVendorClaude,
				Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: "/nonexistent/freeside-test-claude-instructions",
			},
		},
		time.Now,
	)
	if err := option(&Engine{}); err == nil {
		t.Fatal("WithAdmission accepted a prompt digest the materializer cannot resolve")
	}
}

func TestWithAdmissionRejectsUnconformedVendorInstructionBinding(t *testing.T) {
	digest := domain.Digest("sha256:" + strings.Repeat("1", 64))
	for _, tc := range []struct {
		name     string
		vendor   domain.AgentVendor
		delivery domain.VendorInstructionDelivery
	}{
		{"unknown vendor", "unknown", domain.VendorInstructionDeliveryAppendFile},
		{"missing binding", domain.AgentVendorCodex, ""},
		{"replace-authority instructions key", domain.AgentVendorCodex, "instructions_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			option := WithAdmission(
				stageInputBackend{}, nil,
				AdmissionEnvironment{
					PromptPackageDigest: digest,
					VendorInstructions: VendorInstructionConfig{
						Vendor: tc.vendor, Delivery: tc.delivery,
						HostPath: "/nonexistent/vendor-instructions",
					},
				},
				time.Now,
			)
			if err := option(&Engine{}); !errors.Is(
				err, domain.ErrUnsupportedVendorInstructionBinding,
			) {
				t.Fatalf("WithAdmission() = %v, want typed binding refusal", err)
			}
		})
	}

	option := WithAdmission(
		stageInputBackend{}, nil,
		AdmissionEnvironment{
			PromptPackageDigest: digest,
			VendorInstructions: VendorInstructionConfig{
				Vendor:   domain.AgentVendorCodex,
				Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: "/nonexistent/AGENTS.md",
			},
		},
		time.Now,
	)
	if err := option(&Engine{}); err != nil {
		t.Fatalf("WithAdmission() rejected the conformed Codex binding: %v", err)
	}
}

func TestWithAdmissionRequiresConfigurationBoundUnattendedBackend(t *testing.T) {
	option := WithAdmission(
		stageInputBackend{}, nil,
		AdmissionEnvironment{
			OperatingMode:       domain.ModeUnattended,
			PromptPackageDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
			VendorInstructions: VendorInstructionConfig{
				Vendor:   domain.AgentVendorClaude,
				Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: "/nonexistent/freeside-test-claude-instructions",
			},
		},
		time.Now,
	)
	if err := option(&Engine{}); err == nil {
		t.Fatal("WithAdmission accepted an unattended backend with no configuration identity")
	}
}

func TestProductionReplayDeliveryDefersToKnownDriverInvocation(t *testing.T) {
	for _, feedback := range []bool{false, true} {
		t.Run(map[bool]string{false: "remediation", true: "operator feedback"}[feedback], func(t *testing.T) {
			runID := domain.RunID("run-replay-delivery")
			invocationID := remediationInvocationID(runID, 1)
			admission := domain.ExecutionAdmission{
				InvocationID: invocationID,
				RunID:        runID,
				StageID:      remediationStageID(runID, 1),
			}
			if feedback {
				invocationID = operatorFeedbackInvocationID("replay")
				admission.InvocationID = invocationID
				admission.StageID = operatorFeedbackStageID(invocationID)
			}
			refusal := errors.Join(ErrProductionInputUndeliverable, exec.ErrInputTooLarge)
			validationCalls := 0
			e := &Engine{
				driver: replayInspectionDriver{
					inspection: exec.Inspection{Status: exec.StatusRunning, Live: true},
				},
				productionDeliveryValidator: func(context.Context, exec.StartSpec) error {
					validationCalls++
					return refusal
				},
			}
			if err := e.validateProductionReplayDelivery(t.Context(), invocationID, admission); err != nil {
				t.Fatalf("known driver invocation = %v", err)
			}
			if validationCalls != 0 {
				t.Fatalf("known driver invocation revalidated delivery %d times", validationCalls)
			}

			e.driver = replayInspectionDriver{err: exec.ErrUnknownInvocation}
			if err := e.validateProductionReplayDelivery(t.Context(), invocationID, admission); !errors.Is(err, refusal) {
				t.Fatalf("unknown driver invocation = %v, want delivery refusal", err)
			}
			if validationCalls != 1 {
				t.Fatalf("unknown driver invocation validation calls = %d, want 1", validationCalls)
			}
		})
	}
}

// seedRunPolicy stores the run with a resolved policy and returns it bound to
// that policy's digest. Writer-stage admission reads the run's policy, so a
// fixture that admits one stores it first. values adds keys to the one every
// seeded policy carries.
func seedRunPolicy(t *testing.T, st *store.Store, run domain.Run, values map[string]string) domain.Run {
	t.Helper()
	provenance := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:test-policy"}
	keys := []domain.PolicyKey{{Key: "rein", Value: "tight", Provenance: provenance}}
	for key, value := range values {
		keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
	}
	policy, err := domain.NewResolvedPolicy(run.ID, keys)
	if err != nil {
		t.Fatal(err)
	}
	run.PolicyDigest = policy.Digest
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), run); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(t.Context(), policy)
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// The writer's egress profile comes from the run's resolved policy, and a
// profile the composition cannot enforce is refused rather than recorded.
// The production composition declares only provider_only enforceable, so
// until the proxy enforces the registry set (#1628) a policy that opts in to
// provider_registry admits nothing.
func TestWriterAdmissionTakesTheEgressProfileFromPolicy(t *testing.T) {
	const registrySet = `["proxy.golang.org","sum.golang.org"]`
	optIn := map[string]string{
		domain.EgressProfilePolicyKey: string(domain.EgressProviderRegistry),
		domain.RegistrySetPolicyKey:   registrySet,
	}
	providerOnly := []domain.EgressProfile{domain.EgressProviderOnly}
	withRegistry := []domain.EgressProfile{domain.EgressProviderOnly, domain.EgressProviderRegistry}
	for name, tc := range map[string]struct {
		policy      map[string]string
		enforceable []domain.EgressProfile
		want        domain.EgressProfile
		wantErr     error
	}{
		"no key admits provider_only": {
			policy: nil, enforceable: providerOnly, want: domain.EgressProviderOnly,
		},
		"a declared set alone admits provider_only": {
			policy:      map[string]string{domain.RegistrySetPolicyKey: registrySet},
			enforceable: withRegistry, want: domain.EgressProviderOnly,
		},
		"opt-in is refused when only provider_only is enforceable": {
			policy: optIn, enforceable: providerOnly, wantErr: ErrEgressProfileNotEnforceable,
		},
		"opt-in is admitted when the composition enforces it": {
			policy: optIn, enforceable: withRegistry, want: domain.EgressProviderRegistry,
		},
		"opt-in without a registry set is a policy error": {
			policy: map[string]string{
				domain.EgressProfilePolicyKey: string(domain.EgressProviderRegistry),
			},
			enforceable: withRegistry, wantErr: domain.ErrRegistrySetInvalid,
		},
		"a profile the key cannot select is a policy error": {
			policy: map[string]string{
				domain.EgressProfilePolicyKey: string(domain.EgressProviderWebRead),
			},
			enforceable: []domain.EgressProfile{domain.EgressProviderOnly, domain.EgressProviderWebRead},
			wantErr:     domain.ErrInvalidEgressProfile,
		},
		"a profile the domain does not know is a policy error": {
			policy: map[string]string{
				domain.EgressProfilePolicyKey: "provider_everything",
			},
			enforceable: withRegistry, wantErr: ErrEgressPolicyRefused,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.engine.admission.environment.EnforceableEgressProfiles = tc.enforceable
			run := seedRunPolicy(t, f.store, domain.Run{
				ID: "run-egress", ProjectID: f.run.ProjectID, SpecDigest: f.run.SpecDigest,
			}, tc.policy)
			stage := domain.Stage{ID: productionStageID(run.ID), RunID: run.ID, Name: productionStageName}
			invocation, err := domain.NewAgentInvocation(
				productionInvocationID(run.ID), []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			admission, admitted, err := f.engine.admitAttempt(
				t.Context(), invocationBinding{run: run, invocation: invocation}, stage, invocation.ID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || admitted {
					t.Fatalf("admitAttempt = admitted %t, err %v; want %v and no admission", admitted, err, tc.wantErr)
				}
				// The refusal is this run's own: the pass holds the one
				// invocation under a typed reason and goes on to the next.
				// Unclassified, it would end the reconcile loop.
				if !invocationDispatchHold(err) || unattendedDispatchRefusal(err) {
					t.Fatalf("refusal %v does not hold its own invocation only", err)
				}
				if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldAdmissionPolicyRefused {
					t.Fatalf("hold reason = %q, %t; want %q", reason, ok, domain.HoldAdmissionPolicyRefused)
				}
				return
			}
			if err != nil || !admitted {
				t.Fatalf("admitAttempt = admitted %t, err %v", admitted, err)
			}
			if admission.EgressProfile != tc.want || admission.PolicyDigest != run.PolicyDigest {
				t.Fatalf("admission = profile %q under policy %q; want %q under %q",
					admission.EgressProfile, admission.PolicyDigest, tc.want, run.PolicyDigest)
			}
		})
	}
}

// A writer attempt whose run has no stored policy, or whose stored policy is
// not the one the run names, has no request to read: admission refuses it
// instead of assuming the default.
func TestWriterAdmissionRefusesAnUnreadablePolicy(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	admit := func(run domain.Run) error {
		t.Helper()
		stage := domain.Stage{ID: productionStageID(run.ID), RunID: run.ID, Name: productionStageName}
		invocation, err := domain.NewAgentInvocation(
			productionInvocationID(run.ID), []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, admitted, err := f.engine.admitAttempt(
			t.Context(), invocationBinding{run: run, invocation: invocation}, stage, invocation.ID)
		if admitted {
			t.Fatal("admitAttempt admitted a run whose policy cannot be read")
		}
		return err
	}
	missing := domain.Run{
		ID: "run-no-policy", ProjectID: f.run.ProjectID,
		SpecDigest: f.run.SpecDigest, PolicyDigest: f.run.PolicyDigest,
	}
	if err := admit(missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing policy: err = %v, want store.ErrNotFound", err)
	}
	renamed := f.run
	renamed.PolicyDigest = agentTestDigest("6")
	mismatch := admit(renamed)
	if !errors.Is(mismatch, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("mismatched policy digest: err = %v, want ErrPolicyDigestMismatch", mismatch)
	}
	// Neither is a policy verdict. A run that names a policy the store does
	// not hold is a broken binding, and it keeps the loud failure path.
	for name, err := range map[string]error{"missing": admit(missing), "mismatched": mismatch} {
		if _, held := dispatchHoldReason(err); held || invocationDispatchHold(err) || unattendedDispatchRefusal(err) {
			t.Fatalf("%s policy refusal %v is held quietly", name, err)
		}
	}
}

// Every writer stage reads the request: a remediation round and an
// operator-feedback retry run the same writer as the first production
// attempt, so an opt-in the composition cannot enforce is refused on each. A
// stage that is not the writer's runs under the composition's profile and
// never reads the key.
func TestEgressPolicyGovernsEveryWriterStageAndNoOther(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	f.engine.productionPublication = &productionPublicationWorkflow{
		remediationPromptPackage: agentTestDigest("8"),
	}
	run := seedRunPolicy(t, f.store, domain.Run{
		ID: "run-egress", ProjectID: f.run.ProjectID, SpecDigest: f.run.SpecDigest,
	}, map[string]string{
		domain.EgressProfilePolicyKey: string(domain.EgressProviderRegistry),
		domain.RegistrySetPolicyKey:   `["proxy.golang.org","sum.golang.org"]`,
	})
	admit := func(stage domain.Stage, id domain.InvocationID) (domain.ExecutionAdmission, bool, error) {
		t.Helper()
		invocation, err := domain.NewAgentInvocation(id, []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f.engine.admitAttempt(
			t.Context(), invocationBinding{run: run, invocation: invocation}, stage, id)
	}
	feedbackID := operatorFeedbackInvocationID("command-1")
	for name, tc := range map[string]struct {
		stage domain.StageID
		id    domain.InvocationID
	}{
		"production":        {productionStageID(run.ID), productionInvocationID(run.ID)},
		"remediation":       {remediationStageID(run.ID, 1), remediationInvocationID(run.ID, 1)},
		"operator feedback": {operatorFeedbackStageID(feedbackID), feedbackID},
	} {
		t.Run(name, func(t *testing.T) {
			_, admitted, err := admit(domain.Stage{ID: tc.stage, RunID: run.ID, Name: productionStageName}, tc.id)
			if admitted || !errors.Is(err, ErrEgressProfileNotEnforceable) {
				t.Fatalf("admitAttempt = admitted %t, err %v; want ErrEgressProfileNotEnforceable", admitted, err)
			}
		})
	}
	// The lineup admitter binds ward stages only, so the other stage is
	// admitted under a composition that names its identity directly.
	f.engine.admission.environment.Agents = nil
	f.engine.admission.environment.AuthIdentityID = &f.identity.ID
	admission, admitted, err := admit(domain.Stage{ID: "stage-review", RunID: run.ID, Name: "review"}, "inv-review")
	if err != nil || !admitted {
		t.Fatalf("non-writer stage: admitted %t, err %v", admitted, err)
	}
	if admission.EgressProfile != domain.EgressProviderOnly {
		t.Fatalf("non-writer stage profile = %q, want the composition's provider_only", admission.EgressProfile)
	}
}

// The gate applies to the selected profile, not to how it was selected. A
// capability manifest names a profile and no set, so one that selects
// provider_registry under a policy without a valid declared set is refused
// like the policy opt-in.
func TestWriterEgressRefusalRequiresADeclaredSetForTheRegistryProfile(t *testing.T) {
	policy := func(values map[string]string) domain.ResolvedPolicy {
		t.Helper()
		provenance := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:test-policy"}
		keys := []domain.PolicyKey{{Key: "rein", Value: "tight", Provenance: provenance}}
		for key, value := range values {
			keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
		}
		resolved, err := domain.NewResolvedPolicy("run-egress", keys)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	all := []domain.EgressProfile{
		domain.EgressProviderOnly, domain.EgressProviderRegistry, domain.EgressProviderWebRead,
	}
	declared := policy(map[string]string{domain.RegistrySetPolicyKey: `["pypi.org"]`})
	for name, tc := range map[string]struct {
		enforceable []domain.EgressProfile
		policy      domain.ResolvedPolicy
		profile     domain.EgressProfile
		want        []error
	}{
		"registry profile with a declared set": {all, declared, domain.EgressProviderRegistry, nil},
		"registry profile without a set": {
			all, policy(nil), domain.EgressProviderRegistry,
			[]error{ErrEgressPolicyRefused, domain.ErrRegistrySetInvalid},
		},
		"registry profile with a malformed set": {
			all, policy(map[string]string{domain.RegistrySetPolicyKey: `["PyPI.org"]`}),
			domain.EgressProviderRegistry,
			[]error{ErrEgressPolicyRefused, domain.ErrRegistrySetInvalid},
		},
		"registry profile the composition does not enforce": {
			[]domain.EgressProfile{domain.EgressProviderOnly},
			declared, domain.EgressProviderRegistry,
			[]error{ErrEgressProfileNotEnforceable},
		},
		"another profile needs no set":   {all, policy(nil), domain.EgressProviderWebRead, nil},
		"the default profile needs none": {all, policy(nil), domain.EgressProviderOnly, nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := writerEgressRefusal(tc.enforceable, tc.policy, tc.profile)
			if len(tc.want) == 0 && err != nil {
				t.Fatalf("refused: %v", err)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want %v", err, want)
				}
			}
		})
	}
}

func TestAdmitAttemptResolvesInvocationArtifactsIntoStageRoles(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	blobs, err := signet.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))

	digest := func(fill string) domain.Digest {
		return domain.Digest("sha256:" + strings.Repeat(fill, 64))
	}
	newArtifact := func(id domain.ArtifactID, artifactType domain.ArtifactKind, bodyDigest domain.Digest) domain.Artifact {
		t.Helper()
		artifact, err := domain.NewArtifact(domain.ArtifactInput{
			ID: id, Type: artifactType, Digest: bodyDigest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: "inv-producer",
				HeadBinding:          domain.HeadIndependent,
				SensitivityClass:     domain.SensitivityNormal,
			},
			Metadata: runMeta(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	prior := newArtifact("prior-1", domain.ArtifactKindEvidence, digest("1"))
	image := newArtifact("image-1", imageInputArtifactType, digest("2"))
	specification := newArtifact("spec-1", domain.ArtifactKindSpecification, digest("4"))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, prior); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, image); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, specification)
	}); err != nil {
		t.Fatal(err)
	}
	attachmentDigest := digest("6")
	message, err := domain.NewMessage(
		"message-1", "conversation-1", domain.AuthorUser, "please implement",
		[]domain.Digest{attachmentDigest},
		time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{
		ID: "conversation-1", Status: domain.ConversationIdle,
	}
	conversation, _ = conversation.Append(message)
	conversationID := conversation.ID
	invocation, err := domain.NewAgentInvocation(
		"inv-1", []domain.ArtifactID{prior.ID, image.ID}, &conversationID, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-1")
	vendorBody := []byte("# Host instructions\nStay inside the declared scope.\n")
	vendorPath := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(vendorPath, vendorBody, 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		store: st, signet: attention,
		admission: &admitter{
			backend: stageInputBackend{},
			floor:   []exec.Capability{exec.CapPostExitExport},
			environment: AdmissionEnvironment{
				OperatingMode:       domain.ModeAttendedDev,
				CredentialMode:      domain.CredentialSubscriptionContained,
				EgressProfile:       domain.EgressProviderOnly,
				ImageRef:            domain.ImageRef("agent@sha256:" + strings.Repeat("ab", 32)),
				PromptPackageDigest: digest("3"),
				VendorInstructions: VendorInstructionConfig{
					Vendor:   domain.AgentVendorClaude,
					Delivery: domain.VendorInstructionDeliveryAppendFile,
					HostPath: vendorPath,
				},
				Base: domain.BaseRevision{
					Repo: "owner/repo", RepositoryID: 1,
					BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
				},
				Workspace: "workspace-1", AuthIdentityID: &identity,
				// WithAdmission defaults this set; a directly built admitter
				// states it.
				EnforceableEgressProfiles: []domain.EgressProfile{domain.EgressProviderOnly},
			},
			now: func() time.Time { return time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC) },
		},
	}
	binding := invocationBinding{
		run: seedRunPolicy(t, st, domain.Run{
			ID: "run-1", ProjectID: "project-1", SpecDigest: digest("4"),
		}, nil),
		invocation: invocation, conversation: conversation,
	}
	admission, admitted, err := e.admitAttempt(
		ctx, binding, domain.Stage{ID: "stage-1"}, invocation.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted || admission.StageInputs == nil {
		t.Fatalf("admitted = %t, stage inputs = %v", admitted, admission.StageInputs)
	}
	if got := admission.StageInputs.PriorArtifactDigests; len(got) != 2 ||
		got[0] != prior.Digest || got[1] != attachmentDigest {
		t.Fatalf("prior artifact digests = %v, want [%s %s]",
			got, prior.Digest, attachmentDigest)
	}
	if got := admission.StageInputs.ImageInputDigests; len(got) != 1 || got[0] != image.Digest {
		t.Fatalf("image input digests = %v, want [%s]", got, image.Digest)
	}
	e.specification = &specificationWorkflow{promptPackage: digest("7"), blobs: blobs}
	specificationInvocation, err := domain.NewAgentInvocation(
		"inv-specification", nil, &conversationID, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	specificationBinding := binding
	specificationBinding.run.ID = "specification-run"
	specificationBinding.invocation = specificationInvocation
	specificationAdmission, admitted, err := e.admitAttempt(ctx, specificationBinding, domain.Stage{
		ID: specificationStageID(specificationBinding.run.ID), Name: specificationStageName,
	}, specificationInvocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted || specificationAdmission.StageInputs == nil {
		t.Fatalf("specification admitted = %t, stage inputs = %v",
			admitted, specificationAdmission.StageInputs)
	}
	if got := specificationAdmission.StageInputs.PriorArtifactDigests; len(got) != 0 {
		t.Fatalf("specification prior artifacts include opaque conversation attachment: %v", got)
	}
	if got := specificationAdmission.StageInputs.ImageInputDigests; len(got) != 0 {
		t.Fatalf("specification image inputs claim unsupported attachment delivery: %v", got)
	}
	remediationID := domain.InvocationID("inv-remediate-1-run-1")
	remediationInvocation, err := domain.NewAgentInvocation(
		remediationID, []domain.ArtifactID{prior.ID}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	remediationBinding := binding
	remediationBinding.invocation = remediationInvocation
	deliveryRefusal := errors.New("driver rejects rendered prompt")
	validated := false
	e.productionPublication = &productionPublicationWorkflow{
		remediationPromptPackage: digest("8"),
	}
	e.productionDeliveryValidator = func(_ context.Context, spec exec.StartSpec) error {
		validated = spec.RunID == remediationBinding.run.ID &&
			spec.StageID == remediationStageID(remediationBinding.run.ID, 1) &&
			spec.StageInputs != nil &&
			spec.StageInputs.PromptPackageDigest == digest("8")
		return errors.Join(ErrProductionInputUndeliverable, deliveryRefusal)
	}
	if _, admitted, err := e.admitAttempt(ctx, remediationBinding, domain.Stage{
		ID: remediationStageID(remediationBinding.run.ID, 1), Name: productionStageName,
	}, remediationID); admitted || !validated ||
		!errors.Is(err, ErrRemediationInputUndeliverable) ||
		!errors.Is(err, deliveryRefusal) {
		t.Fatalf("remediation admission = admitted %t, validated %t, err %v", admitted, validated, err)
	}
	e.productionDeliveryValidator = func(context.Context, exec.StartSpec) error {
		return exec.ErrInputUnavailable
	}
	if _, admitted, err := e.admitAttempt(ctx, remediationBinding, domain.Stage{
		ID: remediationStageID(remediationBinding.run.ID, 1), Name: productionStageName,
	}, remediationID); admitted || !errors.Is(err, exec.ErrInputUnavailable) ||
		errors.Is(err, ErrProductionInputUndeliverable) ||
		errors.Is(err, ErrRemediationInputUndeliverable) {
		t.Fatalf("transient remediation admission = admitted %t, err %v", admitted, err)
	}
	operatorFeedbackID := operatorFeedbackInvocationID("command-1")
	operatorFeedbackInvocation, err := domain.NewAgentInvocation(
		operatorFeedbackID, []domain.ArtifactID{specification.ID, prior.ID}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	operatorFeedbackBinding := binding
	operatorFeedbackBinding.invocation = operatorFeedbackInvocation
	validated = false
	e.productionDeliveryValidator = func(_ context.Context, spec exec.StartSpec) error {
		validated = spec.StageID == operatorFeedbackStageID(operatorFeedbackID) && spec.StageInputs != nil &&
			spec.StageInputs.PromptPackageDigest == digest("8")
		return errors.Join(ErrProductionInputUndeliverable, deliveryRefusal)
	}
	if _, admitted, err := e.admitAttempt(ctx, operatorFeedbackBinding, domain.Stage{
		ID: operatorFeedbackStageID(operatorFeedbackID), Name: productionStageName,
	}, operatorFeedbackID); admitted || !validated || !errors.Is(err, ErrProductionInputUndeliverable) {
		t.Fatalf("operator-feedback delivery refusal = admitted %t, validated %t, err %v", admitted, validated, err)
	}
	e.productionDeliveryValidator = func(context.Context, exec.StartSpec) error { return nil }
	operatorFeedbackAdmission, admitted, err := e.admitAttempt(ctx, operatorFeedbackBinding, domain.Stage{
		ID: operatorFeedbackStageID(operatorFeedbackID), Name: productionStageName,
	}, operatorFeedbackID)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted || operatorFeedbackAdmission.StageInputs == nil {
		t.Fatalf("operator-feedback admitted = %t, stage inputs = %v",
			admitted, operatorFeedbackAdmission.StageInputs)
	}
	if got := operatorFeedbackAdmission.StageInputs.PromptPackageDigest; got != digest("8") {
		t.Fatalf("operator-feedback prompt package = %s, want %s", got, digest("8"))
	}
	if got := operatorFeedbackAdmission.StageInputs.PriorArtifactDigests; len(got) != 1 || got[0] != prior.Digest {
		t.Fatalf("operator-feedback prior artifacts = %v, want [%s]", got, prior.Digest)
	}
	if admission.StageInputs.ConversationDigest == nil {
		t.Fatal("conversation-bound admission has no conversation digest")
	}
	if admission.StageInputs.VendorInstructions == nil ||
		admission.StageInputs.VendorInstructions.Digest == nil {
		t.Fatal("admission did not bind the configured host vendor instructions")
	}
	vendorReader, err := blobs.Open(*admission.StageInputs.VendorInstructions.Digest)
	if err != nil {
		t.Fatal(err)
	}
	storedVendor, readErr := io.ReadAll(vendorReader)
	if err := errors.Join(readErr, vendorReader.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedVendor, vendorBody) {
		t.Fatal("stored vendor instructions differ from admitted host bytes")
	}
	if err := os.WriteFile(vendorPath, []byte("changed after admission\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replayedVendor, err := blobs.Open(*admission.StageInputs.VendorInstructions.Digest)
	if err != nil {
		t.Fatal(err)
	}
	replayedBody, readErr := io.ReadAll(replayedVendor)
	if err := errors.Join(readErr, replayedVendor.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayedBody, vendorBody) {
		t.Fatal("host drift changed the admitted vendor-instruction replay")
	}
	wantDigest, wantBody, err := conversation.PrefixContent(1)
	if err != nil {
		t.Fatal(err)
	}
	if *admission.StageInputs.ConversationDigest != wantDigest {
		t.Fatalf("conversation digest = %s, want %s",
			*admission.StageInputs.ConversationDigest, wantDigest)
	}
	body, err := blobs.Open(wantDigest)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, wantBody) {
		t.Fatal("stored conversation prefix differs from admitted canonical bytes")
	}

	malformed := newArtifact("malformed-1", domain.ArtifactKindEvidence, "sha256:not-hex")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, malformed)
	}); err != nil {
		t.Fatal(err)
	}
	badInvocation, err := domain.NewAgentInvocation(
		"inv-bad", []domain.ArtifactID{malformed.ID}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	badBinding := binding
	badBinding.invocation = badInvocation
	badBinding.conversation = domain.Conversation{}
	if _, _, err := e.admitAttempt(
		ctx, badBinding, domain.Stage{ID: "stage-1"}, badInvocation.ID,
	); !errors.Is(err, domain.ErrStageInputsNotCanonical) {
		t.Fatalf("malformed artifact admission = %v, want %v",
			err, domain.ErrStageInputsNotCanonical)
	}
}
