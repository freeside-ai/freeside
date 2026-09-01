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
	runID := domain.RunID("run-replay-delivery")
	invocationID := remediationInvocationID(runID, 1)
	admission := domain.ExecutionAdmission{
		InvocationID: invocationID,
		RunID:        runID,
		StageID:      remediationStageID(runID, 1),
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
}

func TestAdmitAttemptResolvesInvocationArtifactsIntoStageRoles(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
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
			},
			now: func() time.Time { return time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC) },
		},
	}
	binding := invocationBinding{
		run: domain.Run{
			ID: "run-1", ProjectID: "project-1",
			SpecDigest: digest("4"), PolicyDigest: digest("5"),
		},
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
	e.elaboration = &elaborationWorkflow{promptPackage: digest("7"), blobs: blobs}
	elaborationInvocation, err := domain.NewAgentInvocation(
		"inv-elaboration", nil, &conversationID, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	elaborationBinding := binding
	elaborationBinding.run.ID = "elaboration-run"
	elaborationBinding.invocation = elaborationInvocation
	elaborationAdmission, admitted, err := e.admitAttempt(ctx, elaborationBinding, domain.Stage{
		ID: elaborationStageID(elaborationBinding.run.ID), Name: elaborationStageName,
	}, elaborationInvocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted || elaborationAdmission.StageInputs == nil {
		t.Fatalf("elaboration admitted = %t, stage inputs = %v",
			admitted, elaborationAdmission.StageInputs)
	}
	if got := elaborationAdmission.StageInputs.PriorArtifactDigests; len(got) != 0 {
		t.Fatalf("elaboration prior artifacts include opaque conversation attachment: %v", got)
	}
	if got := elaborationAdmission.StageInputs.ImageInputDigests; len(got) != 0 {
		t.Fatalf("elaboration image inputs claim unsupported attachment delivery: %v", got)
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
