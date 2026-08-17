package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	elaboratefake "github.com/freeside-ai/freeside/daemon/internal/elaborate/fake"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type submitElaborationBackend struct{}

func (submitElaborationBackend) Name() string { return "submit-elaboration-test" }

func (submitElaborationBackend) Capabilities() exec.CapabilitySet {
	return exec.NewCapabilitySet(exec.CapPostExitExport)
}

func TestSubmitCommandElaboratesBeforeCreatingProductionRun(t *testing.T) {
	tests := []struct {
		name           string
		acceptedBody   string
		wantSameDigest bool
	}{
		{
			name:           "byte-identical specification",
			acceptedBody:   "# Work item\n\nImplement the thing.",
			wantSameDigest: true,
		},
		{
			name:           "revised specification",
			acceptedBody:   "# Work item\n\nImplement the improved thing.",
			wantSameDigest: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testSubmitCommandElaborationDigest(t, tc.acceptedBody, tc.wantSameDigest)
		})
	}
}

func testSubmitCommandElaborationDigest(t *testing.T, acceptedBody string, wantSameDigest bool) {
	t.Helper()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	sourceBody := "# Work item\n\nImplement the thing."
	if err := os.WriteFile(specPath, []byte(sourceBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), SpecPath: specPath,
		PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "project-submit-elaboration", RunID: "implementation-from-submit",
	}
	submitted, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), cfg.DBPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	attention := signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(func() time.Time { return now }))
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutDevice(t.Context(), domain.Device{
			ID: "device-submit", DisplayName: "Operator", Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(t.Context(), domain.AuthIdentity{
			ID: "auth-submit", Provider: "codex", AuthStoreMutationLease: true,
			AuthStoreVolume: "provider-credentials", MaxParallelExecutions: 1,
			RefreshStrategy: domain.RefreshOnDemand,
		}, now)
	}); err != nil {
		t.Fatal(err)
	}
	promptBody := []byte("Elaborate the submitted work item.\n")
	promptDigest := domain.Digest(contentaddr.Sum(promptBody))
	if _, err := blobs.Put(promptDigest, bytes.NewReader(promptBody)); err != nil {
		t.Fatal(err)
	}
	vendorPath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(vendorPath, []byte("Stay within the submitted work unit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver, err := execfake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatal(err)
	}
	if err := elaboratefake.Script(driver, submitted.ElaborationInvocationID, 0, 0, elaborate.Output{
		Specification: &elaborate.Specification{
			Summary:    "Ready for implementation.",
			Body:       acceptedBody,
			Addressals: []elaborate.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fetcher, err := elaborate.NewFetcher(st, blobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-submit")
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(submitElaborationBackend{}, []exec.Capability{exec.CapPostExitExport}, engine.AdmissionEnvironment{
			OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile:       domain.EgressProviderOnly,
			ImageRef:            domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
			PromptPackageDigest: promptDigest,
			VendorInstructions: engine.VendorInstructionConfig{
				Vendor: domain.AgentVendorCodex, Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: vendorPath,
			},
			Base: domain.BaseRevision{
				Repo: "owner/repo", RepositoryID: 1,
				BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
			},
			Workspace: "workspace-submit", AuthIdentityID: &identity,
		}, func() time.Time { return now }),
		engine.WithElaboration(engine.ElaborationConfig{
			Fetcher: fetcher, Blobs: blobs, Now: func() time.Time { return now },
			PromptPackageDigest: promptDigest,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	itemID := domain.ItemID("spec-approval-" + string(submitted.RunID) + "-1")
	var item domain.AttentionItem
	var snapshot store.Snapshot
	for range 5 {
		if _, err := workflow.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = st.Read(t.Context(), func(tx *store.ReadTx) error {
			item, snapshot, err = tx.GetAttentionItemSnapshot(t.Context(), itemID)
			return err
		})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("approval item was not created: %v", err)
	}
	if _, err := attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "approve-submitted-spec", DeviceID: "device-submit",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionApprove,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	var implementationRun domain.Run
	for range 5 {
		if _, err := workflow.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		err = st.Read(t.Context(), func(tx *store.ReadTx) error {
			implementationRun, err = tx.GetRun(t.Context(), submitted.RunID)
			return err
		})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatalf("approved implementation run was not created: %v", err)
	}
	if gotSame := implementationRun.SpecDigest == submitted.SourceDigest; gotSame != wantSameDigest {
		t.Fatalf("implementation/source digest equality = %t, want %t (%q / %q)",
			gotSame, wantSameDigest, implementationRun.SpecDigest, submitted.SourceDigest)
	}
	if len(item.AgentClaims) != 1 || item.AgentClaims[0].Digest != implementationRun.SpecDigest {
		t.Fatalf("approval claim = %+v, want approved implementation digest %q",
			item.AgentClaims, implementationRun.SpecDigest)
	}
	var initialAttempt domain.ProductionAttempt
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		initialAttempt, err = tx.GetProductionAttemptByRun(t.Context(), implementationRun.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if initialAttempt.CampaignID != submitted.CampaignID || initialAttempt.AttemptNumber != 1 ||
		initialAttempt.SourceDigest != submitted.SourceDigest ||
		initialAttempt.PublicationDigest != submitted.PublicationDigest ||
		initialAttempt.ApprovedSpecDigest != implementationRun.SpecDigest {
		t.Fatalf("initial production attempt = %+v, want submitted/approved lineage", initialAttempt)
	}
	if replay, err := workflow.Reconcile(t.Context()); err != nil || replay.RunTransitions != 0 {
		t.Fatalf("approval replay = %+v, %v", replay, err)
	}
	resubmitted, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatalf("re-submit after byte-identical specification approval: %v", err)
	}
	if resubmitted != submitted {
		t.Fatalf("re-submit result = %#v, want full elaboration result %#v", resubmitted, submitted)
	}
	if _, err := engine.ReattemptProductionRun(t.Context(), st, engine.ProductionReattemptSpec{
		ParentRunID: implementationRun.ID, Reason: "retry while live",
	}); err == nil || !strings.Contains(err.Error(), "use resume while it is live") {
		t.Fatalf("live-parent reattempt = %v, want resume refusal", err)
	}
	terminal := domain.ObservedStatusFailed
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: implementationRun.ID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &submitted.ImplementationInvocationID, Terminal: &terminal,
			RecordedAt: now.Add(time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.ReattemptProductionRun(t.Context(), st, engine.ProductionReattemptSpec{
		ParentRunID: implementationRun.ID, Reason: "retry after acceptance rig repair",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Attempt.CampaignID != submitted.CampaignID || retry.Attempt.AttemptNumber != 2 ||
		retry.Attempt.ParentRunID != implementationRun.ID ||
		retry.Attempt.ApprovedSpecDigest != implementationRun.SpecDigest ||
		retry.Attempt.SourceDigest != submitted.SourceDigest ||
		retry.Attempt.PublicationDigest != submitted.PublicationDigest ||
		retry.RootSourceArtifactID != submitted.SourceArtifactID ||
		retry.Run.Run.SpecDigest != implementationRun.SpecDigest ||
		retry.Run.Run.ID == implementationRun.ID {
		t.Fatalf("retry = %+v, want new attempt over unchanged approved/source digests", retry)
	}
}
