package signet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestFailureRetryGate: Submit accepts a retry on a failure card only when
// its reconciler can act on it (#3440). The command id must be trimmed for
// every retry action, and a plain Retry needs facts that still offer it.
func TestFailureRetryGate(t *testing.T) {
	t.Parallel()
	role := domain.RoleImplementer
	offering := domain.ExecutionFailureFacts{
		Outcome: domain.ExecutionOutcomeLost, Stage: domain.StageNameImplementation,
		InvocationID: "inv-implement", RetryOutlook: domain.RetryOutlookMaySucceed,
		Admitted: &domain.InvocationAdmissionFacts{
			InvocationID: "inv-implement", Kind: domain.AdmissionKindExecution, Role: &role,
		},
	}
	// A card written before #3440 lists Retry with no outlook.
	old := offering
	old.RetryOutlook = ""
	repeats := offering
	repeats.RetryOutlook = domain.RetryOutlookWouldRepeat
	card := func(facts domain.ExecutionFailureFacts) domain.AttentionItem {
		return domain.AttentionItem{ID: "item-execution_failure-inv-implement", ExecutionFailure: &facts}
	}
	cases := []struct {
		name    string
		command domain.Command
		item    domain.AttentionItem
		refused bool
	}{
		{"offered retry", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(offering), false},
		{"untrimmed retry id", domain.Command{CommandID: "c1 ", Action: domain.ActionRetry}, card(offering), true},
		{
			"untrimmed capability retry id",
			domain.Command{CommandID: " c1", Action: domain.ActionRetryWithCapability},
			card(offering), true,
		},
		{
			"untrimmed alternate agent retry id",
			domain.Command{CommandID: "c1\n", Action: domain.ActionRetryWithAlternateAgent},
			card(offering), true,
		},
		{
			"retry with a message",
			domain.Command{CommandID: "c1", Action: domain.ActionRetry, Message: "again"},
			card(offering), true,
		},
		{"card without an outlook", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(old), true},
		{"would-repeat card", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(repeats), true},
		{"another action", domain.Command{CommandID: "c1 ", Action: domain.ActionStop}, card(old), false},
	}
	for _, tc := range cases {
		err := validateFailureRetry(tc.command, tc.item)
		if tc.refused != errors.Is(err, domain.ErrParentKeyMismatch) || (!tc.refused && err != nil) {
			t.Errorf("%s: validateFailureRetry = %v, refused want %v", tc.name, err, tc.refused)
		}
	}
}

type failureRetryFixture struct {
	store   *store.Store
	service *Service
	now     time.Time
	taskID  domain.TaskID
	runID   domain.RunID
}

func newFailureRetryFixture(t *testing.T, withTask bool, opts ...Option) failureRetryFixture {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	st := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	allOpts := append([]Option{WithClock(func() time.Time { return now })}, opts...)
	svc := NewService(st, allOpts...)
	f := failureRetryFixture{
		store: st, service: svc, now: now, runID: "run-failure-retry",
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Operator Mac", Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		if withTask {
			task, err := tx.GetOrCreateTask(ctx, "project-1", domain.SpecificationSource{
				Kind: domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{
					Repo: "owner/repo", RepositoryID: 1, IssueNumber: 42,
				},
			})
			if err != nil {
				return err
			}
			f.taskID = task.ID
		}
		return tx.PutRun(ctx, domain.Run{
			ID: f.runID, TaskID: f.taskID, ProjectID: "project-1",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		})
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *failureRetryFixture) seedFailureInvocation(
	t *testing.T, invocationID domain.InvocationID, stage domain.StageName,
	outcome domain.ExecutionOutcomeStatus, cause domain.FailureCause,
) {
	t.Helper()
	ctx := t.Context()
	stageID := domain.StageID("stage-" + string(invocationID))
	attemptID := domain.AttemptID("attempt-" + string(invocationID))
	invocation, err := domain.NewAgentInvocation(
		invocationID, []domain.ArtifactID{domain.ArtifactID("input-" + invocationID)}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := invocation.ComputeInputDigest()
	if err != nil {
		t.Fatal(err)
	}
	identityID := domain.AuthIdentityID("auth-" + string(invocationID))
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, f.runID)
		if err != nil {
			return err
		}
		run.Stages = append(run.Stages, domain.Stage{
			ID: stageID, RunID: f.runID, Name: string(stage),
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		})
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		if err := storetest.RecordIdentityOnPool(ctx, tx, domain.AuthIdentity{
			ID: identityID, Provider: domain.AuthProviderOpenAI, AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-credentials", RefreshStrategy: domain.RefreshOnDemand},
		}, 8, f.now); err != nil {
			return err
		}
		admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
			InvocationID: invocationID, RunID: f.runID, StageID: stageID, AttemptID: attemptID,
			Backend: "fresh_vm_read_only_volume_handoff", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile: domain.EgressProviderOnly,
			ImageRef:      domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
			SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
			Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
			Workspace: "workspace-1", AuthIdentityID: &identityID, AdmittedAt: f.now,
		})
		if err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: invocationID, AdmissionID: admission.ID,
			Status: outcome, Cause: cause, Summary: "execution ended", RecordedAt: f.now.Add(time.Second),
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *failureRetryFixture) putSpecificationCard(t *testing.T) AttentionItemSnapshot {
	t.Helper()
	invocationID := domain.InvocationID("inv-spec-retry")
	f.seedFailureInvocation(t, invocationID, domain.StageNameSpecification, domain.ExecutionOutcomeCanceled, "")
	role := domain.RoleSpecifier
	itemID := domain.ExecutionFailureItemID(invocationID)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.runID), RunID: &f.runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            "specification canceled",
		RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionDiscuss, domain.ActionStop},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeCanceled, Stage: domain.StageNameSpecification,
			InvocationID: invocationID, RetryOutlook: domain.RetryOutlookMaySucceed,
			Admitted: &domain.InvocationAdmissionFacts{
				InvocationID: invocationID, Kind: domain.AdmissionKindExecution, Role: &role,
				AdmittedAt: f.now,
			},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.PutItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	snap, err := f.service.GetAttentionItem(t.Context(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func (f *failureRetryFixture) stopTask(t *testing.T, state domain.TaskCancellationState) {
	t.Helper()
	ctx := t.Context()
	serverState, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var c domain.TaskCancellation
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		receipt, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "stop-task-" + string(state), DeviceID: "device-1",
			TaskID: f.taskID, ProjectID: "project-1",
			ExpectedSyncEpoch: serverState.SyncEpoch, ExpectedEntityVersion: serverState.Revision,
		}, f.now.Add(2*time.Second))
		if err != nil {
			return err
		}
		c = receipt.Cancellation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state == domain.TaskCancellationRequested {
		return
	}
	ack := domain.TaskCancellationAcknowledgement{
		ID: "ack-" + string(state), RequestID: c.RequestID, TargetDigest: c.TargetDigest,
		State: state, EvidenceDigest: c.TargetDigest, RecordedAt: f.now.Add(3 * time.Second),
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AcknowledgeTaskCancellation(ctx, ack)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *failureRetryFixture) assertRefusedUnchanged(t *testing.T, commandID string, before AttentionItemSnapshot, revBefore int64, err error) {
	t.Helper()
	if !errors.Is(err, ErrTaskStopped) {
		t.Fatalf("Submit = %v, want ErrTaskStopped", err)
	}
	state, stateErr := f.store.ServerState(t.Context())
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if state.Revision != revBefore {
		t.Fatalf("server revision moved from %d to %d on refusal", revBefore, state.Revision)
	}
	if readErr := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, _, getErr := tx.GetCommandSnapshot(t.Context(), commandID)
		return getErr
	}); !errors.Is(readErr, store.ErrNotFound) {
		t.Fatalf("GetCommandSnapshot(%q) = %v, want ErrNotFound", commandID, readErr)
	}
	after, getErr := f.service.GetAttentionItem(t.Context(), before.Item.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.Item.Status != domain.StatusOpen || after.Item.DecidedAt != nil ||
		after.Item.ItemVersion != before.Item.ItemVersion || after.EntityVersion != before.EntityVersion {
		t.Fatalf("card after refusal = status %s item_version %d entity_version %d, want open at %d/%d",
			after.Item.Status, after.Item.ItemVersion, after.EntityVersion, before.Item.ItemVersion, before.EntityVersion)
	}
}

// TestFailureRetryStoppedTaskRefusesPlainRetry checks the signet Stop gate for
// plain Retry across all three Stop states, with no Stop, and with no task.
func TestFailureRetryStoppedTaskRefusesPlainRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		withTask bool
		stop     domain.TaskCancellationState
		refused  bool
	}{
		{name: "no stop", withTask: true},
		{name: "run without task", withTask: false},
		{name: "stop requested", withTask: true, stop: domain.TaskCancellationRequested, refused: true},
		{name: "stop confirmed", withTask: true, stop: domain.TaskCancellationConfirmed, refused: true},
		{name: "stop failed_to_stop", withTask: true, stop: domain.TaskCancellationFailed, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFailureRetryFixture(t, tc.withTask)
			snap := f.putSpecificationCard(t)
			if tc.stop != "" {
				f.stopTask(t, tc.stop)
			}
			revBefore, err := f.store.ServerState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			const commandID = "cmd-plain-retry"
			_, err = f.service.Submit(t.Context(), ClientCommand{
				CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: snap.EntityVersion,
				Payload: DecisionPayload{
					ItemID: snap.Item.ID, Action: domain.ActionRetry, ItemVersion: snap.Item.ItemVersion,
				},
			})
			if tc.refused {
				f.assertRefusedUnchanged(t, commandID, snap, revBefore.Revision, err)
				return
			}
			if err != nil {
				t.Fatalf("Submit without Stop = %v, want accepted", err)
			}
			after, err := f.service.GetAttentionItem(t.Context(), snap.Item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Item.Status != domain.StatusResolved {
				t.Fatalf("card status = %s, want resolved", after.Item.Status)
			}
		})
	}
}

// TestFailureRetryStoppedTaskRefusesOtherRetries verifies the same Stop gate
// refuses retry_with_capabilities, retry_with_alternate_agent, and operator
// feedback Retry, while leaving closed/stale 409 precedence intact.
func TestFailureRetryStoppedTaskRefusesOtherRetries(t *testing.T) {
	t.Parallel()

	t.Run("retry_with_capabilities", func(t *testing.T) {
		t.Parallel()
		f := newFailureRetryFixture(t, true)
		manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
		if err != nil {
			t.Fatal(err)
		}
		invocationID := domain.InvocationID("inv-impl-cap-stop")
		f.seedFailureInvocation(t, invocationID, domain.StageNameImplementation, domain.ExecutionOutcomeFailed, domain.FailureCauseUnknown)
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ExecutionFailureItemID(invocationID), ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.runID), RunID: &f.runID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason:            "implementation failed",
			RequestedDecision: []domain.Action{domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseUnknown,
				Stage: domain.StageNameImplementation, InvocationID: invocationID,
				OfferedManifests: []domain.CapabilityManifestOffer{manifest.Offer()},
			},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.PutItem(t.Context(), item); err != nil {
			t.Fatal(err)
		}
		snap, err := f.service.GetAttentionItem(t.Context(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.stopTask(t, domain.TaskCancellationRequested)
		revBefore, err := f.store.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		digest := manifest.Digest
		const commandID = "cmd-cap-retry-stopped"
		_, err = f.service.Submit(t.Context(), ClientCommand{
			CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: snap.EntityVersion,
			Payload: DecisionPayload{
				ItemID: snap.Item.ID, Action: domain.ActionRetryWithCapability,
				ItemVersion: snap.Item.ItemVersion, CapabilityManifestDigest: &digest,
			},
		})
		f.assertRefusedUnchanged(t, commandID, snap, revBefore.Revision, err)
	})

	t.Run("retry_with_alternate_agent", func(t *testing.T) {
		t.Parallel()
		retrierCalled := false
		f := newFailureRetryFixture(t, true, WithAlternateAgentRetrier(func(
			context.Context, *store.WriteTx, domain.Command, domain.AttentionItem, time.Time,
		) (domain.AlternateAgentRetry, error) {
			retrierCalled = true
			return domain.AlternateAgentRetry{}, errors.New("retrier must not run on a stopped task")
		}))
		const enrollment domain.ClientEnrollmentID = "anthropic-a/claude_code"
		invocationID := domain.InvocationID("inv-impl-alt-stop")
		f.seedFailureInvocation(t, invocationID, domain.StageNameImplementation, domain.ExecutionOutcomeFailed, domain.FailureCauseQuota)
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ExecutionFailureItemID(invocationID), ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.runID), RunID: &f.runID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason:            "implementation hit quota",
			RequestedDecision: []domain.Action{domain.ActionRetryWithAlternateAgent, domain.ActionDiscuss, domain.ActionStop},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseQuota,
				Stage: domain.StageNameImplementation, InvocationID: invocationID,
			},
			AlternateAgents: []domain.AlternateAgentOffer{enrollmentOffer(enrollment)},
			ItemVersion:     1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.PutItem(t.Context(), item); err != nil {
			t.Fatal(err)
		}
		snap, err := f.service.GetAttentionItem(t.Context(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.stopTask(t, domain.TaskCancellationRequested)
		revBefore, err := f.store.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		const commandID = "cmd-alt-retry-stopped"
		_, err = f.service.Submit(t.Context(), ClientCommand{
			CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: snap.EntityVersion,
			Payload: DecisionPayload{
				ItemID: snap.Item.ID, Action: domain.ActionRetryWithAlternateAgent,
				ItemVersion:          snap.Item.ItemVersion,
				AlternateAgentChoice: &AlternateAgentChoice{EnrollmentID: enrollment},
			},
		})
		if retrierCalled {
			t.Fatal("alternate-agent retrier ran on a stopped task")
		}
		f.assertRefusedUnchanged(t, commandID, snap, revBefore.Revision, err)
	})

	t.Run("operator_feedback_retry", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newFailureRetryFixture(t, true)
		sourceInvID := domain.InvocationID("inv-implement-root")
		feedbackInvID := domain.InvocationID("inv-operator-feedback-return-1")
		feedbackStageID := domain.StageID("operator-feedback-" + string(feedbackInvID))
		headSHA := strings.Repeat("2", 40)
		baseSHA := strings.Repeat("1", 40)
		inputArtID := domain.ArtifactID("operator-feedback-return-1")
		inputDigest := domain.Digest("sha256:" + strings.Repeat("b", 64))
		readyID := domain.ProductionReadyItemID(f.runID)
		decidedAt := f.now.Add(time.Second)

		sourceInv, err := domain.NewAgentInvocation(sourceInvID, []domain.ArtifactID{"input-root"}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sourceInputDigest, err := sourceInv.ComputeInputDigest()
		if err != nil {
			t.Fatal(err)
		}
		feedbackInv, err := domain.NewAgentInvocation(feedbackInvID, []domain.ArtifactID{"input-root", inputArtID}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		feedbackInputDigest, err := feedbackInv.ComputeInputDigest()
		if err != nil {
			t.Fatal(err)
		}
		intentPayload, err := json.Marshal(domain.OperatorFeedbackInvocationIntent{
			Version:             domain.OperatorFeedbackInvocationIntentVersion,
			InvocationID:        feedbackInvID,
			RunID:               f.runID,
			StageID:             feedbackStageID,
			CommandID:           "return-1",
			ItemID:              readyID,
			SourceInvocationID:  sourceInvID,
			InputArtifactID:     inputArtID,
			InputArtifactDigest: inputDigest,
			BaseSHA:             baseSHA,
			HeadSHA:             headSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		readyItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: readyID, ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.runID), RunID: &f.runID},
			Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
			Reason:            "ready for review",
			RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionStop},
			PRHeadSHA:         headSHA,
			PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 7},
			ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
			Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		returnCmd, err := domain.NewCommand(domain.CommandInput{
			CommandID: "return-1", DeviceID: "device-1", ItemID: readyID,
			ItemVersion: 1, PRHeadSHA: headSHA, Action: domain.ActionReturnToAgent,
			Message: "fix the wording",
		})
		if err != nil {
			t.Fatal(err)
		}
		failureCard, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ExecutionFailureItemID(feedbackInvID), ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.runID), RunID: &f.runID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason:            "feedback failed",
			RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionDiscuss, domain.ActionStop},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseUnknown,
				Stage: domain.StageNameImplementation, InvocationID: feedbackInvID,
			},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		identityID := domain.AuthIdentityID("auth-feedback")
		pubInvID := domain.ProductionPublicationInvocationID(f.runID)
		pubIdentity, err := publish.DeriveIdentity(publish.IdentityInput{
			Repo: "owner/repo", BaseRef: "refs/heads/main", SourceHeadSHA: headSHA,
			ArtifactDigests: []domain.Digest{"sha256:ready-artifact"},
		})
		if err != nil {
			t.Fatal(err)
		}
		pubIntentPayload, err := (publish.Intent{
			FormatVersion: publish.IntentFormatCurrent,
			Identity:      pubIdentity.Digest(), InvocationID: pubInvID,
			Branch: pubIdentity.BranchName(), Repo: "owner/repo",
			BaseRef: "refs/heads/main", SourceHeadSHA: headSHA,
			AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("ef", 32)),
			ProducingInvocationID: sourceInvID, ReservationRunID: f.runID,
		}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		pubIntentKey, err := publish.IntentKey(pubInvID, publish.IntentKindPublication)
		if err != nil {
			t.Fatal(err)
		}
		pubOutcomePayload, err := (publish.Outcome{
			Identity: pubIdentity.Digest(), Repo: "owner/repo", BaseRef: "refs/heads/main",
			HeadSHA: headSHA, Branch: pubIdentity.BranchName(), PRNumber: 7, EvidenceEligible: true,
		}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		readyBinding := domain.ReadyItemPRBinding{
			ItemID: readyID, RunID: f.runID, ProducingInvocationID: sourceInvID,
			PublicationInvocationID: pubInvID, PublicationIdentity: pubIdentity.Digest(),
			Repo: "owner/repo", RepositoryID: 1, PRNumber: 7,
			BaseRef: "refs/heads/main", HeadSHA: headSHA, RecordedAt: f.now,
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			run, err := tx.GetRun(ctx, f.runID)
			if err != nil {
				return err
			}
			run.Stages = []domain.Stage{
				{
					ID: "implement-root", RunID: f.runID, Name: "implement",
					Attempts: []domain.Attempt{{ID: "att-root", StageID: "implement-root", Number: 1, InvocationID: sourceInvID}},
				},
				{
					ID: feedbackStageID, RunID: f.runID, Name: "implement",
					Attempts: []domain.Attempt{{ID: "att-fb", StageID: feedbackStageID, Number: 1, InvocationID: feedbackInvID}},
				},
			}
			if err := tx.PutRun(ctx, run); err != nil {
				return err
			}
			if err := storetest.RecordIdentityOnPool(ctx, tx, domain.AuthIdentity{
				ID: identityID, Provider: domain.AuthProviderOpenAI, AuthStoreMutationLease: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-credentials", RefreshStrategy: domain.RefreshOnDemand},
			}, 8, f.now); err != nil {
				return err
			}
			for _, inv := range []domain.AgentInvocation{sourceInv, feedbackInv} {
				if err := tx.PutAgentInvocation(ctx, inv); err != nil {
					return err
				}
			}
			for _, adm := range []struct {
				inv    domain.InvocationID
				stage  domain.StageID
				att    domain.AttemptID
				digest domain.Digest
			}{
				{sourceInvID, "implement-root", "att-root", sourceInputDigest},
				{feedbackInvID, feedbackStageID, "att-fb", feedbackInputDigest},
			} {
				admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
					InvocationID: adm.inv, RunID: f.runID, StageID: adm.stage, AttemptID: adm.att,
					Backend: "fresh_vm_read_only_volume_handoff", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
					OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
					EgressProfile: domain.EgressProviderOnly,
					ImageRef:      domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
					SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: adm.digest,
					Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: baseSHA},
					Workspace: "workspace-1", AuthIdentityID: &identityID, AdmittedAt: f.now,
				})
				if err != nil {
					return err
				}
				if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
					return err
				}
				if adm.inv == sourceInvID {
					exportRec, err := domain.NewExecutionExport(domain.ExecutionExportInput{
						InvocationID: sourceInvID, AdmissionID: admission.ID,
						ObservedBaseSHA: baseSHA, HeadSHA: headSHA,
						ManifestDigest: "sha256:ready-manifest", RecordedAt: f.now,
					})
					if err != nil {
						return err
					}
					if err := tx.RecordExecutionExport(ctx, exportRec); err != nil {
						return err
					}
				}
				if adm.inv == feedbackInvID {
					if err := tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
						InvocationID: feedbackInvID, AdmissionID: admission.ID,
						Status: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseUnknown,
						Summary: "feedback launch failed", RecordedAt: f.now.Add(time.Second),
					}); err != nil {
						return err
					}
				}
			}
			if _, _, err := tx.EnqueueOutbox(ctx, pubIntentKey, publish.IntentKindPublication, pubIntentPayload); err != nil {
				return err
			}
			if err := tx.MarkOutboxDispatched(ctx, pubIntentKey); err != nil {
				return err
			}
			if _, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(pubIdentity), publish.IntentKindOutcome, pubOutcomePayload); err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, readyItem); err != nil {
				return err
			}
			if err := tx.PutCommand(ctx, returnCmd); err != nil {
				return err
			}
			readyItem, err = readyItem.ConcludeByCommand(domain.StatusSuperseded, decidedAt, domain.ConclusionDecision{
				CommandID: returnCmd.CommandID, Action: returnCmd.Action,
				DeviceID: returnCmd.DeviceID, DeviceDisplayName: "Operator Mac",
			})
			if err != nil {
				return err
			}
			readyItem.ItemVersion++
			if err := tx.PutAttentionItem(ctx, readyItem); err != nil {
				return err
			}
			if err := tx.PutArtifact(ctx, domain.Artifact{
				ID: inputArtID, Type: domain.ArtifactKindEvidence, Digest: inputDigest,
				Provenance: domain.Provenance{
					ProducerClass: domain.ProducerDaemon, ProducerInvocationID: sourceInvID,
					HeadBinding: domain.HeadBound, SourceHeadSHA: headSHA,
					SensitivityClass: domain.SensitivityNormal,
				},
				Metadata: domain.EvidenceMetadata{
					MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1,
					CreatedAt: f.now, Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
				},
			}); err != nil {
				return err
			}
			if _, _, err := tx.EnqueueOutbox(ctx, string(feedbackInvID), string(domain.OperatorFeedbackInvocationRequestedKind), intentPayload); err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, failureCard)
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordReadyItemPRBinding(ctx, readyBinding)
		}); err != nil {
			t.Fatal(err)
		}
		// Prove the feedback parent authenticates before the Stop, so the refusal
		// below comes from the Stop gate rather than OperatorFeedbackRetryParent.
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.OperatorFeedbackRetryParent(ctx, failureCard)
			return err
		}); err != nil {
			t.Fatalf("seed feedback parent invalid: %v", err)
		}
		snap, err := f.service.GetAttentionItem(ctx, failureCard.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.stopTask(t, domain.TaskCancellationRequested)
		revBefore, err := f.store.ServerState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		const commandID = "cmd-feedback-retry-stopped"
		_, err = f.service.Submit(ctx, ClientCommand{
			CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: snap.EntityVersion,
			Payload: DecisionPayload{
				ItemID: snap.Item.ID, Action: domain.ActionRetry, ItemVersion: snap.Item.ItemVersion,
			},
		})
		f.assertRefusedUnchanged(t, commandID, snap, revBefore.Revision, err)
	})

	t.Run("stale_version_keeps_409_precedence", func(t *testing.T) {
		t.Parallel()
		f := newFailureRetryFixture(t, true)
		snap := f.putSpecificationCard(t)
		f.stopTask(t, domain.TaskCancellationRequested)
		_, err := f.service.Submit(t.Context(), ClientCommand{
			CommandID: "cmd-stale-stopped", DeviceID: "device-1",
			ExpectedEntityVersion: snap.EntityVersion + 1,
			Payload: DecisionPayload{
				ItemID: snap.Item.ID, Action: domain.ActionRetry, ItemVersion: snap.Item.ItemVersion,
			},
		})
		var stale *StaleVersionError
		if !errors.As(err, &stale) {
			t.Fatalf("stale Submit on stopped task = %v, want *StaleVersionError", err)
		}
	})
}

// TestFailureRetryReplayAfterStopReturnsRecordedResult verifies that a Retry
// accepted before a Stop replays its recorded result after the Stop arrives.
func TestFailureRetryReplayAfterStopReturnsRecordedResult(t *testing.T) {
	t.Parallel()
	f := newFailureRetryFixture(t, true)
	snap := f.putSpecificationCard(t)
	cmd := ClientCommand{
		CommandID: "cmd-retry-before-stop", DeviceID: "device-1", ExpectedEntityVersion: snap.EntityVersion,
		Payload: DecisionPayload{
			ItemID: snap.Item.ID, Action: domain.ActionRetry, ItemVersion: snap.Item.ItemVersion,
		},
	}
	first, err := f.service.Submit(t.Context(), cmd)
	if err != nil {
		t.Fatalf("initial Submit before Stop: %v", err)
	}
	f.stopTask(t, domain.TaskCancellationRequested)
	replay, err := f.service.Submit(t.Context(), cmd)
	if err != nil {
		t.Fatalf("replay Submit after Stop: %v", err)
	}
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay = %+v, want %+v", replay, first)
	}
}
