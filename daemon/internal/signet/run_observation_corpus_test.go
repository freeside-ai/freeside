package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This file is the returned-object boundary's forge corpus: the single place
// that enumerates every projected fact authenticateRunObservation proves,
// against every binding field an adversary could forge. Each milestone, item,
// and observation kind has one valid baseline (the projection the daemon
// actually writes) and a set of forge cases that mutate one binding field and
// require the read to fail closed. The fixture list, not prose, is the spec of
// what "inside the returned-object boundary" means; the standing disposition in
// devlog/2026-08-12-0710-run-observation-projection.md governs anything a
// later finding claims is missing.
//
// The line the corpus draws (per that note's "Returned-Object Boundary
// Contract"): a single-record fetch plus field-equality binding is inside the
// boundary and appears here; a multi-record reconstruction or engine-recovery
// derivation (recoverDefinitiveBlockedTask, compatibleTerminalItem) is outside
// it and is deliberately absent.

const (
	corpusRepositoryID = int64(424242)
	corpusBaseSHA      = "deadbeef"
)

// corpusFixture is a signet service over an admission-capable store: the
// attended-dev admission floor and approved credential mode let the corpus
// build the real durable-authority records (admissions, exports, outcomes)
// that back the terminal milestones, rather than only the missing-authority
// forges the bare fixture can reach.
type corpusFixture struct {
	store   *store.Store
	service *signet.Service
	at      time.Time
}

func newCorpusFixture(t *testing.T, opts ...signet.Option) corpusFixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, t.TempDir()+"/corpus.db", store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	at := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	readAt := at.Add(time.Hour)
	svc := signet.NewService(s, append([]signet.Option{
		signet.WithClock(func() time.Time { return readAt }),
	}, opts...)...)
	return corpusFixture{store: s, service: svc, at: at}
}

// read exercises the full authenticated projection the same way ListRuns,
// GetRun, and GetRunTimeline do; every corpus assertion goes through it so a
// forge that only one accessor catches would still be caught here.
func (f corpusFixture) read(ctx context.Context, runID domain.RunID) error {
	if _, err := f.service.GetRunTimeline(ctx, runID); err != nil {
		return err
	}
	if _, err := f.service.GetRun(ctx, runID); err != nil {
		return err
	}
	if _, err := f.service.ListRuns(ctx); err != nil {
		return err
	}
	return nil
}

func (f corpusFixture) mustWrite(t *testing.T, mutate func(tx *store.WriteTx) error) {
	t.Helper()
	if err := f.store.Write(context.Background(), mutate); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (f corpusFixture) mustWriteInternal(t *testing.T, mutate func(tx *store.InternalTx) error) {
	t.Helper()
	if err := f.store.WriteInternal(context.Background(), mutate); err != nil {
		t.Fatalf("write internal: %v", err)
	}
}

func corpusRun(runID domain.RunID, attempts ...domain.Attempt) domain.Run {
	stage := domain.Stage{
		ID: domain.StageID("stage-" + string(runID)), RunID: runID, Name: "implementation",
		Attempts: attempts,
	}
	return domain.Run{
		ID: runID, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{stage},
	}
}

func corpusAttempt(runID domain.RunID, invocation domain.InvocationID) domain.Attempt {
	return domain.Attempt{
		ID: domain.AttemptID("attempt-" + string(invocation)), StageID: domain.StageID("stage-" + string(runID)),
		Number: 1, InvocationID: invocation,
	}
}

func (f corpusFixture) seedAuthIdentity(t *testing.T) {
	t.Helper()
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.RecordAuthIdentity(context.Background(), domain.AuthIdentity{
			ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
			AuthStoreVolume: "provider-cred", MaxParallelExecutions: 1,
			RefreshStrategy: domain.RefreshOnDemand,
		}, f.at)
	})
}

// seedAdmission records a real attended-dev admission for one attempt, which
// also appends the invocation_admitted milestone the run projects.
func (f corpusFixture) seedAdmission(
	t *testing.T, runID domain.RunID, stageID domain.StageID, attemptID domain.AttemptID, invocation domain.InvocationID,
) domain.ExecutionAdmission {
	t.Helper()
	identityID := domain.AuthIdentityID("auth-1")
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocation, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     "sha256:spec", PolicyDigest: "sha256:policy", InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: corpusRepositoryID, BaseRef: "refs/heads/main", BaseSHA: corpusBaseSHA,
		},
		Workspace: "ws-1", AuthIdentityID: &identityID,
		AdmittedAt: f.at,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.RecordExecutionAdmission(context.Background(), admission)
	})
	return admission
}

func (f corpusFixture) seedExport(t *testing.T, admission domain.ExecutionAdmission) {
	t.Helper()
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: admission.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.RecordExecutionExportRecord(context.Background(), export)
	})
}

func (f corpusFixture) seedOutcome(
	t *testing.T, admission domain.ExecutionAdmission, status domain.ExecutionOutcomeStatus,
) {
	t.Helper()
	summary := "attempt did not converge"
	if status == domain.ExecutionOutcomeLost {
		summary = ""
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(context.Background(), domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: status, Summary: summary, RecordedAt: admission.AdmittedAt.Add(time.Hour),
		})
	})
}

func (f corpusFixture) appendMilestone(t *testing.T, milestone domain.RunMilestone) {
	t.Helper()
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(context.Background(), milestone)
	})
}

func (f corpusFixture) observe(t *testing.T, observation domain.InvocationObservation) {
	t.Helper()
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.RecordInvocationObservation(context.Background(), observation)
	})
}

func (f corpusFixture) seedItem(t *testing.T, item domain.AttentionItem) {
	t.Helper()
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(context.Background(), item)
	})
}

func ptr[T any](v T) *T { return &v }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func productionIntentPayload(invocation domain.InvocationID, runID domain.RunID, stageID domain.StageID) []byte {
	return []byte(fmt.Sprintf(`{"invocation_id":%q,"run_id":%q,"stage_id":%q}`, invocation, runID, stageID))
}

func corpusStageID(runID domain.RunID) domain.StageID {
	return domain.StageID("stage-" + string(runID))
}

func publicationInvocation(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("publish-production-" + string(runID))
}

// seedConversationStart records the durable authority a conversation-lane
// invocation_started milestone binds to: a dispatched agent_invocation_requested
// outbox entry, the agent invocation that carries the conversation id, and the
// conversation-bound attention item the intent names. Its run/stage/attempt
// half comes from the admission the caller seeds separately.
func (f corpusFixture) seedConversationStart(
	t *testing.T, runID domain.RunID, invocation domain.InvocationID, conversation domain.ConversationID, itemID domain.ItemID,
) {
	t.Helper()
	ctx := context.Background()
	intent := domain.ConversationInvocationIntent{
		InvocationID: invocation, ConversationID: conversation, ItemID: itemID, ItemVersion: 1,
	}
	payload, err := jsonMarshal(intent)
	if err != nil {
		t.Fatalf("marshal conversation intent: %v", err)
	}
	agentInvocation, err := domain.NewAgentInvocation(invocation, nil, &conversation, 1)
	if err != nil {
		t.Fatalf("NewAgentInvocation: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the agent needs a decision to proceed",
		RequestedDecision: []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		ConversationID: &conversation, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem(conversation): %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, domain.Conversation{ID: conversation, Status: domain.ConversationIdle}); err != nil {
			return err
		}
		return tx.PutAgentInvocation(ctx, agentInvocation)
	})
	f.seedItem(t, item)
	f.mustWriteInternal(t, func(tx *store.InternalTx) error {
		entry, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.AgentInvocationRequestedKind), payload)
		if err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
	})
}

func (f corpusFixture) readyItem(runID domain.RunID) (domain.AttentionItem, error) {
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
}

// corpusPublicationIdentity is the content-addressed publication identity every
// ready-binding fixture shares; its branch name is derived, not free-form.
var corpusPublicationIdentity = domain.Digest("sha256:" + strings.Repeat("a", 64))

// seedReadyBinding records the full durable publication record set a ready item
// binds to: the producing admission and export, the dispatched publication
// intent, and the converged publication outcome, then the binding itself. Both
// the write gate and GetReadyItemPRBinding re-anchor the binding to every one
// of those records, so a valid ready baseline needs them all present and
// field-consistent.
func (f corpusFixture) seedReadyBinding(
	t *testing.T, runID domain.RunID, producing, publication domain.InvocationID,
) {
	t.Helper()
	ctx := context.Background()
	identity := corpusPublicationIdentity
	intent := publicationrecord.Intent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      identity, InvocationID: publication,
		Repo: "owner/repo", BaseRef: "refs/heads/main", SourceHeadSHA: "cafebabe",
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: producing, ReservationRunID: runID,
	}
	intentPayload, err := intent.Encode()
	if err != nil {
		t.Fatalf("encode publication intent: %v", err)
	}
	outcome := publicationrecord.Outcome{
		Identity: identity, Repo: "owner/repo", BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		Branch: publicationrecord.BranchName(identity), PRNumber: 123, EvidenceEligible: true,
	}
	outcomePayload, err := jsonMarshal(outcome)
	if err != nil {
		t.Fatalf("encode publication outcome: %v", err)
	}
	intentKey := "publish/" + string(publication) + "/publish.publication"
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, "publish.publication", intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", outcomePayload); err != nil {
			return err
		}
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: domain.ProductionReadyItemID(runID), RunID: runID,
			ProducingInvocationID:   producing,
			PublicationInvocationID: publication,
			PublicationIdentity:     identity,
			Repo:                    "owner/repo", RepositoryID: corpusRepositoryID, PRNumber: 123,
			BaseRef: "refs/heads/main", HeadSHA: "cafebabe", RecordedAt: f.at.Add(time.Hour),
		})
	})
}

func blockedItem(runID domain.RunID, reason string, actions []domain.Action) (domain.AttentionItem, error) {
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionBlockedItemID(runID), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: actions,
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
}

// TestRunObservationCorpusValidBaselines is the corpus's positive half: each
// milestone, item, and observation kind, built the way the daemon writes it,
// authenticates. A forge case is only meaningful if its baseline passes, so a
// regression that broke a baseline (rather than the boundary) surfaces here
// instead of hiding as a green forge.
func TestRunObservationCorpusValidBaselines(t *testing.T) {
	ctx := context.Background()

	t.Run("run_submitted", func(t *testing.T) {
		f := newCorpusFixture(t)
		runID := domain.RunID("run-submitted")
		invocation := domain.InvocationID("inv-submitted")
		f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, corpusRun(runID)) })
		f.mustWriteInternal(t, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
				productionIntentPayload(invocation, runID, corpusStageID(runID)))
			return err
		})
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid run_submitted baseline: %v", err)
		}
	})

	t.Run("invocation_admitted", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-admitted")
		invocation := domain.InvocationID("inv-admitted")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid invocation_admitted baseline: %v", err)
		}
	})

	t.Run("invocation_started_conversation", func(t *testing.T) {
		f := newCorpusFixture(t)
		runID := domain.RunID("run-started")
		invocation := domain.InvocationID("inv-started")
		// A conversation (discuss) invocation is dispatched unbound: it is a
		// run attempt with a started milestone but no execution admission, so
		// the baseline seeds none. Its deterministic attempt identity is the
		// only run/stage/attempt binding the boundary can re-derive.
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		f.seedConversationStart(t, runID, invocation, "conv-1", "conv-item-1")
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at.Add(2 * time.Minute),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid conversation invocation_started baseline: %v", err)
		}
	})

	t.Run("execution_export_recorded", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-export")
		invocation := domain.InvocationID("inv-export")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		f.seedExport(t, admission)
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid execution_export_recorded baseline: %v", err)
		}
	})

	t.Run("running_observation_behind_export", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-export-lag")
		invocation := domain.InvocationID("inv-export-lag")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		f.seedExport(t, admission)
		f.observe(t, domain.InvocationObservation{
			InvocationID: invocation, RunID: runID, Status: domain.ObservedStatusRunning,
			Live: true, ObservedAt: f.at.Add(time.Hour),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid lagging export observation baseline: %v", err)
		}
		timeline, err := f.service.GetRunTimeline(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.Invocations) != 1 ||
			timeline.Invocations[0].Status != domain.ObservedStatusCompleted || timeline.Invocations[0].Live {
			t.Fatalf("served invocation = %+v, want completed and not live", timeline.Invocations)
		}
	})

	t.Run("execution_outcome_recorded", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-outcome")
		invocation := domain.InvocationID("inv-outcome")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		f.seedOutcome(t, admission, domain.ExecutionOutcomeFailed)
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid execution_outcome_recorded baseline: %v", err)
		}
	})

	t.Run("gone_observation_behind_failed_terminal", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-failed-lag")
		invocation := domain.InvocationID("inv-failed-lag")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		f.seedOutcome(t, admission, domain.ExecutionOutcomeFailed)
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
			Terminal: ptr(domain.ObservedStatusFailed), RecordedAt: f.at.Add(2 * time.Hour),
		})
		f.observe(t, domain.InvocationObservation{
			InvocationID: invocation, RunID: runID, Status: domain.ObservedStatusGone,
			Live: false, ObservedAt: f.at.Add(3 * time.Hour),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid lagging failed observation baseline: %v", err)
		}
		timeline, err := f.service.GetRunTimeline(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.Invocations) != 1 || timeline.Invocations[0].Status != domain.ObservedStatusFailed {
			t.Fatalf("served invocation = %+v, want failed", timeline.Invocations)
		}
	})

	t.Run("terminal_recorded", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-terminal")
		invocation := domain.InvocationID("inv-terminal")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
		})
		admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
		f.seedExport(t, admission)
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
			Terminal: ptr(domain.ObservedStatusCompleted), RecordedAt: f.at.Add(2 * time.Hour),
		})
		f.observe(t, domain.InvocationObservation{
			InvocationID: invocation, RunID: runID, Status: domain.ObservedStatusCompleted,
			Live: false, ObservedAt: f.at.Add(2 * time.Hour),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid terminal_recorded baseline: %v", err)
		}
	})

	t.Run("publication_ready", func(t *testing.T) {
		f := newCorpusFixture(t)
		f.seedAuthIdentity(t)
		runID := domain.RunID("run-ready")
		attemptInvocation := domain.InvocationID("inv-ready-attempt")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, attemptInvocation)))
		})
		producingAdmission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, attemptInvocation).ID, attemptInvocation)
		f.seedExport(t, producingAdmission)
		item, err := f.readyItem(runID)
		if err != nil {
			t.Fatalf("readyItem: %v", err)
		}
		f.seedItem(t, item)
		f.seedReadyBinding(t, runID, attemptInvocation, publicationInvocation(runID))
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: ptr(publicationInvocation(runID)),
			RecordedAt: f.at.Add(3 * time.Hour),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid publication_ready baseline: %v", err)
		}
	})

	t.Run("publication_blocked", func(t *testing.T) {
		f := newCorpusFixture(t)
		runID := domain.RunID("run-blocked")
		attemptInvocation := domain.InvocationID("inv-blocked-attempt")
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, attemptInvocation)))
		})
		item, err := blockedItem(runID, domain.PublicationBlockTrust,
			[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
		if err != nil {
			t.Fatalf("blockedItem: %v", err)
		}
		f.seedItem(t, item)
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
			Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid publication_blocked baseline: %v", err)
		}
	})

	t.Run("hold_on_reserved_invocation", func(t *testing.T) {
		f := newCorpusFixture(t)
		runID := domain.RunID("run-held")
		invocation := domain.InvocationID("inv-held")
		// A run submitted but refused admission (here an identity-parallelism
		// limit) holds its reserved invocation before any attempt exists. The
		// hold binds under the reserved outbox intent, not an attempt.
		f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, corpusRun(runID)) })
		f.mustWriteInternal(t, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
				productionIntentPayload(invocation, runID, corpusStageID(runID)))
			return err
		})
		f.appendMilestone(t, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
		})
		f.mustWrite(t, func(tx *store.WriteTx) error {
			return tx.RecordRunHold(ctx, domain.RunHoldObservation{
				RunID: runID, InvocationID: &invocation, Reason: domain.HoldIdentityParallelism,
				FirstObservedAt: f.at, LastObservedAt: f.at.Add(time.Minute),
			})
		})
		if err := f.read(ctx, runID); err != nil {
			t.Fatalf("valid hold-on-reserved baseline: %v", err)
		}
	})
}

// seedForeignRunAdmission records a valid admission for invocation, bound to a
// separate real run. A milestone on the run under test that names that same
// invocation then fails authenticateAdmissionRun's run/stage/attempt equality
// while every backing record still exists: the forge is the retargeting, not a
// missing authority.
func (f corpusFixture) seedForeignRunAdmission(t *testing.T, invocation domain.InvocationID) domain.ExecutionAdmission {
	t.Helper()
	foreign := domain.RunID("run-foreign-" + string(invocation))
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutRun(context.Background(), corpusRun(foreign, corpusAttempt(foreign, invocation)))
	})
	return f.seedAdmission(t, foreign, corpusStageID(foreign), corpusAttempt(foreign, invocation).ID, invocation)
}

// assertFailClosed requires the projection to reject the forge with a
// fail-closed sentinel, never a nil error and never an unrelated failure that
// would let a forge pass for the wrong reason.
func assertFailClosed(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("forge %q authenticated; want fail-closed", name)
	}
	if !errors.Is(err, domain.ErrParentKeyMismatch) && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forge %q failed with %v; want ErrParentKeyMismatch or ErrNotFound", name, err)
	}
}

type forgeCase struct {
	// name is "<kind>/<forged binding field>".
	name string
	// build assembles a fixture valid except for one forged binding and
	// returns the run under test.
	build func(t *testing.T, f corpusFixture) domain.RunID
}

// TestRunObservationCorpusForges is the adversarial half: every binding field
// authenticateRunObservation proves, forged independently against an otherwise
// valid fixture, must fail the read closed. The case list is the enumeration of
// what the boundary defends; a check not represented here is a coverage gap by
// construction. Cases that would require reconstructing a second durable record
// to forge (an execution-record status corrupted below the writer that couples
// it to its milestone, a snapshot revision rewritten under the store) are
// deliberately absent: they sit on the recovery-owned side of the note's
// boundary line, or are pinned by the store package's own tamper tests
// (internal/store/observation_internal_test.go).
func TestRunObservationCorpusForges(t *testing.T) {
	ctx := context.Background()
	cases := []forgeCase{
		// run_submitted: reserved outbox production intent bound to a declared
		// run stage, before any attempt exists.
		{"run_submitted/missing_intent", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, corpusRun(runID)) })
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},
		{"run_submitted/intent_names_another_run", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, corpusRun(runID)) })
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
					productionIntentPayload(invocation, "run-other", corpusStageID(runID)))
				return err
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},
		{"run_submitted/intent_names_another_stage", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, corpusRun(runID)) })
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
					productionIntentPayload(invocation, runID, "stage-other"))
				return err
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},

		// invocation_admitted: durable admission binding invocation to
		// run/stage/attempt.
		{"invocation_admitted/missing_admission", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationAdmitted, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},
		{"invocation_admitted/admission_bound_to_another_run", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.seedForeignRunAdmission(t, invocation)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationAdmitted, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},

		// invocation_started: attempt membership, a dispatched outbox entry,
		// the recognized dispatch intent, the conversation-item binding, and,
		// for an unbound conversation invocation (no admission, no run/stage in
		// the intent), its deterministic attempt identity.
		{"invocation_started/undispatched_entry", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
					productionIntentPayload(invocation, runID, corpusStageID(runID)))
				return err
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at.Add(time.Minute),
			})
			return runID
		}},
		{"invocation_started/payload_bound_to_another_run", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				entry, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
					productionIntentPayload(invocation, "run-other", corpusStageID(runID)))
				if err != nil {
					return err
				}
				return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at.Add(time.Minute),
			})
			return runID
		}},
		// The 815 case: a run graph corrupted so an existing conversation
		// invocation is attached to an attempt that is not its own. A
		// conversation invocation is dispatched unbound (no admission), and its
		// dispatch intent carries no run/stage, so the dispatch-intent and
		// conversation-item checks pass; only the deterministic attempt identity
		// (attempt-<invocation>) catches the retargeting.
		{"invocation_started/conversation_attempt_retargeted", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			retargeted := domain.Attempt{
				ID: "attempt-forged", StageID: corpusStageID(runID), Number: 1, InvocationID: invocation,
			}
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, retargeted))
			})
			f.seedConversationStart(t, runID, invocation, "conv-1", "conv-item-1")
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at.Add(time.Minute),
			})
			return runID
		}},

		// execution_export_recorded: an export record plus the admission
		// binding.
		{"execution_export_recorded/missing_record", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneExecutionExportRecorded, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},
		{"execution_export_recorded/admission_bound_to_another_run", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			foreign := f.seedForeignRunAdmission(t, invocation)
			f.seedExport(t, foreign)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneExecutionExportRecorded, InvocationID: &invocation, RecordedAt: f.at,
			})
			return runID
		}},

		// execution_outcome_recorded: an outcome record whose status equals the
		// milestone's, plus the admission binding.
		{"execution_outcome_recorded/missing_record", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneExecutionOutcomeRecorded, InvocationID: &invocation,
				Outcome: ptr(domain.ExecutionOutcomeFailed), RecordedAt: f.at,
			})
			return runID
		}},
		{"execution_outcome_recorded/status_disagrees_with_record", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
			// Record the milestone as canceled first so first-observation-wins
			// keeps it, then record a failed outcome whose auto-milestone is
			// deduplicated; the projected milestone now disagrees with its
			// record.
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneExecutionOutcomeRecorded, InvocationID: &invocation,
				Outcome: ptr(domain.ExecutionOutcomeCanceled), RecordedAt: f.at,
			})
			f.seedOutcome(t, admission, domain.ExecutionOutcomeFailed)
			return runID
		}},
		{"execution_outcome_recorded/admission_bound_to_another_run", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			foreign := f.seedForeignRunAdmission(t, invocation)
			f.seedOutcome(t, foreign, domain.ExecutionOutcomeFailed)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneExecutionOutcomeRecorded, InvocationID: &invocation,
				Outcome: ptr(domain.ExecutionOutcomeFailed), RecordedAt: f.at,
			})
			return runID
		}},

		// terminal_recorded: authenticateTerminal against run/stage/attempt and
		// terminal-value agreement with the export or outcome authority.
		{"terminal_recorded/unbound_invocation", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
				Terminal: ptr(domain.ObservedStatusCompleted), RecordedAt: f.at,
			})
			return runID
		}},
		{"terminal_recorded/value_disagrees_with_authority", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
			})
			admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
			f.seedOutcome(t, admission, domain.ExecutionOutcomeFailed)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
				Terminal: ptr(domain.ObservedStatusCanceled), RecordedAt: f.at.Add(2 * time.Hour),
			})
			return runID
		}},

		// publication_ready: the dedicated publish invocation, a workflow-owned
		// ready item, and a ready PR binding whose publication invocation
		// matches.
		{"publication_ready/substituted_invocation", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			attempt := domain.InvocationID("inv-attempt")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, attempt)))
			})
			producing := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, attempt).ID, attempt)
			f.seedExport(t, producing)
			item, err := f.readyItem(runID)
			if err != nil {
				t.Fatalf("readyItem: %v", err)
			}
			f.seedItem(t, item)
			f.seedReadyBinding(t, runID, attempt, publicationInvocation(runID))
			// The milestone names the implementation attempt, not the dedicated
			// publish invocation.
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: &attempt, RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},
		{"publication_ready/missing_binding", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			item, err := f.readyItem(runID)
			if err != nil {
				t.Fatalf("readyItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: ptr(publicationInvocation(runID)),
				RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},
		{"publication_ready/generic_item_id", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			// A ready item whose ID is not the workflow-owned production-ready
			// ID is not run authority; the milestone then has no ready binding.
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: "generic-ready", ProjectID: "proj-1",
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
				Reason:            "checks are green and the diff is ready",
				RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
				PRHeadSHA:         "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
				ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: ptr(publicationInvocation(runID)),
				RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},

		// publication_blocked: the dedicated publish invocation and a
		// definitive, workflow-owned blocked item (exact ID, definitive reason,
		// exact requested-decision shape) whose reason the milestone names.
		{"publication_blocked/reason_mismatch", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			item, err := blockedItem(runID, domain.PublicationBlockTrust,
				[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
			if err != nil {
				t.Fatalf("blockedItem: %v", err)
			}
			f.seedItem(t, item)
			// The item authenticates HoldTrustBlocked; the milestone claims a
			// different definitive reason.
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldRecipeRevoked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},
		{"publication_blocked/non_definitive_reason", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			// A transient publication hold's prose is not in the definitive
			// reason map, so the item cannot authenticate any blocked outcome.
			item, err := blockedItem(runID, "A transient environmental failure held publication for a bounded retry.",
				[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
			if err != nil {
				t.Fatalf("blockedItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},
		{"publication_blocked/wrong_action_shape", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			// The definitive block shape requires exactly inspect-trust-failure
			// then stop; a single action is a transient item.
			item, err := blockedItem(runID, domain.PublicationBlockTrust,
				[]domain.Action{domain.ActionInspectTrustFailure})
			if err != nil {
				t.Fatalf("blockedItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},
		{"publication_blocked/generic_item_id", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: "generic-block", ProjectID: "proj-1",
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
				Reason:            domain.PublicationBlockTrust,
				RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
				ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},

		{"publication_blocked/invocation_reused_by_attempt", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			// The dedicated publish invocation is also an implementation
			// attempt: a publication decision cannot be an ordinary attempt.
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, publicationInvocation(runID))))
			})
			item, err := blockedItem(runID, domain.PublicationBlockTrust,
				[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
			if err != nil {
				t.Fatalf("blockedItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},

		// Item-level: every attention item weighed as run authority re-binds
		// its project, run subject, and subject id before its type-specific
		// evidence can authenticate an outcome.
		{"item_binding/mismatched_run_subject", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			// The workflow-owned blocked ID and RunID, but a subject id naming a
			// different run: the run-binding recheck rejects it.
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: domain.ProductionBlockedItemID(runID), ProjectID: "proj-1",
				Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-other", RunID: &runID},
				Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
				Reason:            domain.PublicationBlockTrust,
				RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
				ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},

		{"item_binding/mismatched_project", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			// The workflow-owned blocked item, correctly subject-bound, but in a
			// different project than the run it claims: the project recheck
			// rejects it.
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: domain.ProductionBlockedItemID(runID), ProjectID: "proj-other",
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
				Reason:            domain.PublicationBlockTrust,
				RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
				ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatalf("NewAttentionItem: %v", err)
			}
			f.seedItem(t, item)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(3 * time.Hour),
			})
			return runID
		}},

		// Observation-level: every invocation observation is an attempt, the
		// hold binds a run-owned invocation, and ready and blocked publication
		// authority never coexist. A status that lags terminal authority is a
		// valid baseline above; the authenticated authority wins when served.
		{"observation/invocation_not_an_attempt", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			f.observe(t, domain.InvocationObservation{
				InvocationID: "inv-stranger", RunID: runID, Status: domain.ObservedStatusRunning,
				Live: false, ObservedAt: f.at,
			})
			return runID
		}},
		{"observation/hold_not_bound_to_run", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			stranger := domain.InvocationID("inv-stranger")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, "inv-attempt")))
			})
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.RecordRunHold(ctx, domain.RunHoldObservation{
					RunID: runID, InvocationID: &stranger, Reason: domain.HoldInputUnavailable,
					FirstObservedAt: f.at, LastObservedAt: f.at.Add(time.Minute),
				})
			})
			return runID
		}},
		{"observation/ready_and_blocked_coexist", func(t *testing.T, f corpusFixture) domain.RunID {
			f.seedAuthIdentity(t)
			runID := domain.RunID("run-1")
			attempt := domain.InvocationID("inv-attempt")
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, attempt)))
			})
			producing := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, attempt).ID, attempt)
			f.seedExport(t, producing)
			ready, err := f.readyItem(runID)
			if err != nil {
				t.Fatalf("readyItem: %v", err)
			}
			f.seedItem(t, ready)
			f.seedReadyBinding(t, runID, attempt, publicationInvocation(runID))
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationReady, InvocationID: ptr(publicationInvocation(runID)),
				RecordedAt: f.at.Add(3 * time.Hour),
			})
			blocked, err := blockedItem(runID, domain.PublicationBlockTrust,
				[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
			if err != nil {
				t.Fatalf("blockedItem: %v", err)
			}
			f.seedItem(t, blocked)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: ptr(publicationInvocation(runID)),
				Reason: ptr(domain.HoldTrustBlocked), RecordedAt: f.at.Add(4 * time.Hour),
			})
			return runID
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCorpusFixture(t)
			runID := tc.build(t, f)
			assertFailClosed(t, tc.name, f.read(ctx, runID))
		})
	}
}

// TestRunObservationCorpusIgnoresUnrelatedItems pins the projection's
// indifference to an attention item that is not run authority: an arbitrary
// other-kind item bound to the run neither authenticates nor blocks the
// projection.
func TestRunObservationCorpusIgnoresUnrelatedItems(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	runID := domain.RunID("run-1")
	invocation := domain.InvocationID("inv-1")
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
	})
	f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
	// An unrelated question item bound to the same run must not change the
	// authenticated projection.
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "unrelated-question", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the agent needs a decision to proceed",
		RequestedDecision: []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	f.seedItem(t, item)
	if err := f.read(ctx, runID); err != nil {
		t.Fatalf("unrelated item changed the projection: %v", err)
	}
}
