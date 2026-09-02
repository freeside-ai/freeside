package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type submitSpecificationBackend struct{}

func (submitSpecificationBackend) Name() string { return "submit-specification-test" }

func (submitSpecificationBackend) Capabilities() exec.CapabilitySet {
	return exec.NewCapabilitySet(exec.CapPostExitExport)
}

func TestSubmitCommandSpecifiesBeforeCreatingProductionRun(t *testing.T) {
	tests := []struct {
		name                  string
		acceptedBody          string
		wantSameDigest        bool
		preallocateRetry      bool
		preallocateForeignRun bool
	}{
		{
			name:             "byte-identical specification",
			acceptedBody:     "# Work item\n\nImplement the thing.",
			wantSameDigest:   true,
			preallocateRetry: true,
		},
		{
			name:           "revised specification",
			acceptedBody:   "# Work item\n\nImplement the improved thing.",
			wantSameDigest: false,
		},
		{
			name:                  "allocated retry run identity collision",
			acceptedBody:          "# Work item\n\nImplement the thing.",
			wantSameDigest:        true,
			preallocateRetry:      true,
			preallocateForeignRun: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testSubmitCommandSpecificationDigest(
				t, tc.acceptedBody, tc.wantSameDigest,
				tc.preallocateRetry, tc.preallocateForeignRun,
			)
		})
	}
}

func testSubmitCommandSpecificationDigest(
	t *testing.T, acceptedBody string, wantSameDigest,
	preallocateRetry, preallocateForeignRun bool,
) {
	t.Helper()
	root := t.TempDir()
	workItemPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := json.Marshal([]domain.CapabilityManifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	var policyKeys []domain.PolicyKey
	if err := json.Unmarshal(
		[]byte(submissionPolicyBody("daemon/**", strings.Repeat("ab", 32))), &policyKeys,
	); err != nil {
		t.Fatal(err)
	}
	policyKeys = append(policyKeys, domain.PolicyKey{
		Key: domain.CapabilityManifestPolicyKey, Value: string(manifestBody),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:capability-policy",
		},
	})
	policyBody, err := json.Marshal(policyKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, policyBody, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceBody := "# Work item\n\nImplement the thing."
	if err := os.WriteFile(workItemPath, []byte(sourceBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), WorkItemPath: workItemPath,
		PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "project-submit-specification", RunID: "implementation-from-submit",
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
			ID: "auth-submit", Provider: "codex", AuthStoreMutationLease: true, MaxParallelExecutions: 2,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-credentials", RefreshStrategy: domain.RefreshOnDemand},
		}, now)
	}); err != nil {
		t.Fatal(err)
	}
	promptBody := []byte("Specify the submitted work item.\n")
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
	if err := specifyfake.Script(driver, submitted.SpecificationInvocationID, 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary:    "Ready for implementation.",
			Body:       acceptedBody,
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fetcher, err := specify.NewFetcher(st, blobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-submit")
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(submitSpecificationBackend{}, []exec.Capability{exec.CapPostExitExport}, engine.AdmissionEnvironment{
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
		engine.WithSpecification(engine.SpecificationConfig{
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
	if len(item.AgentClaims) != 2 || item.AgentClaims[0].Digest != implementationRun.SpecDigest ||
		item.AgentClaims[1].Label != export.SummaryEvidenceLabel ||
		item.AgentClaims[1].Text == nil {
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
		t.Fatalf("re-submit result = %#v, want full specification result %#v", resubmitted, submitted)
	}
	if _, err := engine.ReattemptProductionRun(t.Context(), st, engine.ProductionReattemptSpec{
		ParentRunID: implementationRun.ID, Reason: "retry while live",
	}); err == nil || !strings.Contains(err.Error(), "use resume while it is live") {
		t.Fatalf("live-parent reattempt = %v, want resume refusal", err)
	}
	if len(implementationRun.Stages) != 1 {
		t.Fatalf("implementation stages = %+v, want one", implementationRun.Stages)
	}
	stage := implementationRun.Stages[0]
	attempt := domain.Attempt{
		ID:      domain.AttemptID("attempt-" + string(submitted.ImplementationInvocationID)),
		StageID: stage.ID, Number: 1, InvocationID: submitted.ImplementationInvocationID,
	}
	stage.Attempts = append(stage.Attempts, attempt)
	implementationRun.Stages[0] = stage
	var implementationInputDigest domain.Digest
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		invocation, err := tx.GetAgentInvocation(t.Context(), submitted.ImplementationInvocationID)
		if err != nil {
			return err
		}
		implementationInputDigest, err = invocation.ComputeInputDigest()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: submitted.ImplementationInvocationID, RunID: implementationRun.ID,
		StageID: stage.ID, AttemptID: attempt.ID, Backend: submitSpecificationBackend{}.Name(),
		Capabilities:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		ImageRef:      domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    implementationRun.SpecDigest, PolicyDigest: implementationRun.PolicyDigest,
		InputDigest: implementationInputDigest,
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 1,
			BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "workspace-submit", AuthIdentityID: &identity, AdmittedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), implementationRun); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(t.Context(), admission)
	}); err != nil {
		t.Fatal(err)
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
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(t.Context(), domain.ExecutionOutcome{
			InvocationID: submitted.ImplementationInvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "fixture failure",
			RecordedAt: now.Add(time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	createdAt := now.Add(time.Minute)
	failureItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "execution-failure-command-capability", ProjectID: implementationRun.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(implementationRun.ID), RunID: &implementationRun.ID,
		},
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "implementation failed",
		RequestedDecision: []domain.Action{
			domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
		},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
			InvocationID:     submitted.ImplementationInvocationID,
			OfferedManifests: []domain.CapabilityManifestOffer{manifest.Offer()},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attention.PutItem(t.Context(), failureItem); err != nil {
		t.Fatal(err)
	}
	var failureSnapshot store.Snapshot
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		_, failureSnapshot, err = tx.GetAttentionItemSnapshot(t.Context(), failureItem.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	manifestDigest := manifest.Digest
	if _, err := attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "command-capability", DeviceID: "device-submit",
		ExpectedEntityVersion: failureSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: failureItem.ID, Action: domain.ActionRetryWithCapability,
			ItemVersion:              failureItem.ItemVersion,
			CapabilityManifestDigest: &manifestDigest,
		},
	}); err != nil {
		t.Fatal(err)
	}
	workflowAdmission := engine.AdmissionEnvironment{
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		EnforceableEgressProfiles: []domain.EgressProfile{
			domain.EgressProviderOnly, domain.EgressProviderWebRead,
		},
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
	}
	if preallocateRetry {
		retryRunID, err := engine.ProductionAttemptRunID(submitted.CampaignID, 2)
		if err != nil {
			t.Fatal(err)
		}
		commandID := "command-capability"
		retryOf := submitted.ImplementationInvocationID
		if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
			if preallocateForeignRun {
				if err := tx.PutRun(t.Context(), domain.Run{
					ID: retryRunID, ProjectID: "project-foreign",
					SpecDigest: "sha256:foreign-spec", PolicyDigest: "sha256:foreign-policy",
				}); err != nil {
					return err
				}
			}
			return tx.PutProductionAttempt(t.Context(), domain.ProductionAttempt{
				CampaignID: submitted.CampaignID, AttemptNumber: 2,
				Kind:        domain.ProductionAttemptRetry,
				Reason:      "operator capability retry " + commandID,
				ParentRunID: implementationRun.ID, SourceDigest: submitted.SourceDigest,
				PublicationDigest:   submitted.PublicationDigest,
				ApprovedSpecDigest:  implementationRun.SpecDigest,
				SpecificationRunID:  initialAttempt.SpecificationRunID,
				ImplementationRunID: retryRunID,
				OperatorCommandID:   &commandID, RetryOfInvocationID: &retryOf,
				CapabilityManifestDigest: &manifestDigest,
			})
		}); err != nil {
			t.Fatal(err)
		}
		workflowAdmission.EnforceableEgressProfiles = []domain.EgressProfile{
			domain.EgressProviderOnly,
		}
	}
	retryWorkflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(submitSpecificationBackend{}, []exec.Capability{exec.CapPostExitExport},
			workflowAdmission, func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := retryWorkflow.Reconcile(t.Context())
	if preallocateForeignRun {
		if err == nil || !strings.Contains(err.Error(), "fixed bindings disagree with stored run") {
			t.Fatalf("capability retry collision error = %v", err)
		}
		if replayed, replayErr := retryWorkflow.Reconcile(t.Context()); replayErr == nil ||
			!strings.Contains(replayErr.Error(), "fixed bindings disagree with stored run") {
			t.Fatalf("capability retry collision replay = %+v, %v", replayed, replayErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.RunTransitions != 1 {
		t.Fatalf("capability retry transitions = %+v, want one", reconciled)
	}
	var retry domain.ProductionAttempt
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		retry, err = tx.LatestProductionAttempt(t.Context(), submitted.CampaignID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if retry.CampaignID != submitted.CampaignID || retry.AttemptNumber != 2 ||
		retry.ParentRunID != implementationRun.ID ||
		retry.ApprovedSpecDigest != implementationRun.SpecDigest ||
		retry.SourceDigest != submitted.SourceDigest ||
		retry.PublicationDigest != submitted.PublicationDigest {
		t.Fatalf("retry = %+v, want new attempt over unchanged approved/source digests", retry)
	}
	if retry.OperatorCommandID == nil || *retry.OperatorCommandID != "command-capability" ||
		retry.RetryOfInvocationID == nil || *retry.RetryOfInvocationID != submitted.ImplementationInvocationID ||
		retry.CapabilityManifestDigest == nil || *retry.CapabilityManifestDigest != manifest.Digest {
		t.Fatalf("retry operator bindings = %+v", retry)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetRun(t.Context(), retry.ImplementationRunID)
		return err
	}); err != nil {
		t.Fatalf("capability retry implementation run: %v", err)
	}
	withoutAdmission, err := engine.New(st, attention, driver)
	if err != nil {
		t.Fatal(err)
	}
	restrictedAdmission := workflowAdmission
	restrictedAdmission.EnforceableEgressProfiles = []domain.EgressProfile{domain.EgressProviderOnly}
	withoutSelectedProfile, err := engine.New(st, attention, driver,
		engine.WithAdmission(submitSpecificationBackend{}, []exec.Capability{exec.CapPostExitExport},
			restrictedAdmission, func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, restarted := range map[string]*engine.Engine{
		"admission disabled":       withoutAdmission,
		"selected profile removed": withoutSelectedProfile,
	} {
		t.Run(name, func(t *testing.T) {
			replayed, err := restarted.Reconcile(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if replayed.RunTransitions != 0 {
				t.Fatalf("capability retry replay = %+v, want no transition", replayed)
			}
		})
	}
}
