package engine

import (
	"bytes"
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

type stageInputBackend struct{}

func (stageInputBackend) Name() string { return "stage-input-test" }

func (stageInputBackend) Capabilities() exec.CapabilitySet {
	return exec.NewCapabilitySet(exec.CapPostExitExport)
}

func TestWithAdmissionRejectsNonCanonicalPromptDigest(t *testing.T) {
	option := WithAdmission(
		stageInputBackend{}, nil,
		AdmissionEnvironment{
			PromptPackageDigest: "sha256:not-hex",
			VendorInstructions: VendorInstructionConfig{
				Vendor:   domain.AgentVendorClaude,
				HostPath: "/nonexistent/freeside-test-claude-instructions",
			},
		},
		time.Now,
	)
	if err := option(&Engine{}); err == nil {
		t.Fatal("WithAdmission accepted a prompt digest the materializer cannot resolve")
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
	newArtifact := func(id domain.ArtifactID, artifactType string, bodyDigest domain.Digest) domain.Artifact {
		t.Helper()
		artifact, err := domain.NewArtifact(domain.ArtifactInput{
			ID: id, Type: artifactType, Digest: bodyDigest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: "inv-producer",
				HeadBinding:          domain.HeadIndependent,
				SensitivityClass:     domain.SensitivityNormal,
			},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	prior := newArtifact("prior-1", "report", digest("1"))
	image := newArtifact("image-1", imageInputArtifactType, digest("2"))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, prior); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, image)
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
	if got := admission.StageInputs.ImageInputDigests; len(got) != 1 ||
		got[0] != image.Digest {
		t.Fatalf("image input digests = %v, want [%s]", got, image.Digest)
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

	malformed := newArtifact("malformed-1", "report", "sha256:not-hex")
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
