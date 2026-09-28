package signet_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestReviewEvidenceJSONPreservesRetainedBytes(t *testing.T) {
	events := []byte{'e', 0xff, 0x00, '\n'}
	result := []byte{'r', 0xfe, 0xc0}
	digest := domain.Digest("sha256:" + strings.Repeat("e", 64))
	evidence := signet.ReviewEvidence{Events: events, Result: result, CollectionEvidence: &digest}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var decoded signet.ReviewEvidence
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Events, events) || !bytes.Equal(decoded.Result, result) ||
		decoded.CollectionEvidence == nil || *decoded.CollectionEvidence != digest {
		t.Fatalf("retained bytes or their evidence identity changed: %s", body)
	}
}

func TestRunReviewTimelineProgressAndBindings(t *testing.T) {
	for _, mismatch := range []string{"none", "head", "base", "round", "invocation", "foreign observation"} {
		t.Run(mismatch, func(t *testing.T) {
			ctx := context.Background()
			f := newCorpusFixture(t)
			run := corpusRun("review-run")
			request := domain.ReviewRequestRecord{InvocationID: "review-1", RunID: run.ID, Round: 1, BaseSHA: "base", HeadSHA: "head", RequestedAt: f.at}
			f.mustWrite(t, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				return tx.PutReviewRequest(ctx, request)
			})
			timeline, err := f.service.GetRunTimeline(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if timeline.Review == nil || timeline.Review.Rounds[0].State != domain.ReviewPending {
				t.Fatalf("pending: %#v", timeline.Review)
			}
			before := timeline.AsOfRevision
			observedRun := run.ID
			if mismatch == "foreign observation" {
				observedRun = "foreign"
				other := corpusRun(observedRun)
				f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, other) })
			}
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{InvocationID: request.InvocationID, RunID: observedRun, Status: domain.ObservedStatusRunning, ObservedAt: f.at})
			})
			timeline, err = f.service.GetRunTimeline(ctx, observedRun)
			if mismatch == "foreign observation" {
				if err == nil {
					t.Fatal("foreign review observation accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if timeline.Review.Rounds[0].State != domain.ReviewRunning || timeline.AsOfRevision <= before {
				t.Fatalf("running: %#v", timeline)
			}
			record := domain.ReviewRecord{
				InvocationID: request.InvocationID, RunID: run.ID, Round: 1, Provider: "openai", ModelConfiguration: "codex/high",
				ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), InstructionDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
				CostOwner: "owner", BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, CompletedAt: f.at.Add(time.Minute),
				CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
			}
			switch mismatch {
			case "head":
				record.HeadSHA = "other"
			case "base":
				record.BaseSHA = "other"
			case "round":
				record.Round = 2
			case "invocation":
				record.InvocationID = "other"
			}
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutReviewRecord(ctx, record, nil) })
			timeline, err = f.service.GetRunTimeline(ctx, run.ID)
			if mismatch != "none" {
				if err == nil {
					t.Fatal("mismatched review accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := timeline.Review.Rounds[0]
			if r.State != domain.ReviewCompleted || r.Outcome == nil || *r.Outcome != domain.ReviewClean || r.FindingsCount == nil || *r.FindingsCount != 0 || r.Evidence.Availability != "unavailable" {
				t.Fatalf("completed: %#v", r)
			}
			timeline.AsOf = f.at.Add(time.Hour)
			body, err := json.MarshalIndent(timeline, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, "run-timeline-review", body)
		})
	}
}

func TestReviewCorruptionDoesNotBreakRunListings(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	run := corpusRun("review-corrupt")
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, corpusRun("healthy")); err != nil {
			return err
		}
		return tx.PutReviewRequest(ctx, domain.ReviewRequestRecord{InvocationID: "review-corrupt-1", RunID: run.ID, Round: 1, BaseSHA: "base", HeadSHA: "head", RequestedAt: f.at})
	})
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := db.ExecContext(ctx, "UPDATE review_requests SET body_digest='wrong'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetRunTimeline(ctx, run.ID); err == nil {
		t.Fatal("corrupt request rendered")
	}
	runs, err := f.service.ListRuns(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("run list: %d, %v", len(runs), err)
	}
	if _, err := f.service.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestRunReviewFindingsRereviewAndFailure(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	run := corpusRun("review-history")
	finding := domain.Finding{ID: "finding", RunID: run.ID, Source: "codex_local", Severity: domain.FindingSeverityP2, Message: "Fix the bound implementation", RawText: "Reviewer claim", CreatedAt: f.at}
	record := domain.ReviewRecord{
		InvocationID: "review-history-1", RunID: run.ID, Round: 1, Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), InstructionDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head-1", CompletedAt: f.at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	}
	request := domain.ReviewRequestRecord{InvocationID: "review-history-2", RunID: run.ID, Round: 2, BaseSHA: "base", HeadSHA: "head-2", RequestedAt: f.at.Add(time.Minute)}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRequest(ctx, request); err != nil {
			return err
		}
		return tx.PutReviewRetry(ctx, domain.ReviewRetry{RunID: run.ID, InvocationID: request.InvocationID, Round: 2, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, ObservedAt: request.RequestedAt, Reason: "Retry transport"})
	})
	timeline, err := f.service.GetRunTimeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Review.Rounds) != 2 || timeline.Review.Rounds[0].Dispositions.Open != 1 || timeline.Review.Rounds[0].RequestedAt != nil || !timeline.Review.Rounds[1].RetryPending {
		t.Fatalf("history: %#v", timeline.Review)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.DeleteReviewRetry(ctx, run.ID); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, domain.ReviewFailure{InvocationID: request.InvocationID, RunID: run.ID, Round: 2, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, Class: domain.ReviewFailureConfiguration, Reason: "No diff access", ObservedAt: request.RequestedAt})
	})
	timeline, err = f.service.GetRunTimeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := timeline.Review.Rounds[1]
	if second.State != domain.ReviewFailed || second.Outcome != nil || second.FindingsCount != nil || second.Failure.Reason != "No diff access" || second.RetryPending {
		t.Fatalf("failed: %#v", second)
	}
	if _, err := f.service.GetReviewEvidence(ctx, run.ID, 3); err == nil {
		t.Fatal("unknown round has evidence")
	}
	evidence, err := f.service.GetReviewEvidence(ctx, run.ID, 2)
	if err != nil || evidence.Availability != "unavailable" || evidence.PublishEligible || evidence.SourceHeadSHA != request.HeadSHA {
		t.Fatalf("evidence: %#v, %v", evidence, err)
	}
}

// reviewSubjectFixture seeds a production run whose implementer exported the
// head review round 1 judged. Remediation, adjudication and later rounds are
// layered on by each case.
type reviewSubjectFixture struct {
	corpusFixture
	run        domain.Run
	finding    domain.Finding
	round1     domain.ReviewRecord
	adjudicate domain.FindingAdjudication
}

const (
	subjectImplHead        = "91b3bf9c"
	subjectRemediationHead = "af66c692"
)

func subjectDigest(component string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(component, 64))
}

func newReviewSubjectFixture(t *testing.T) reviewSubjectFixture {
	t.Helper()
	ctx := context.Background()
	f := reviewSubjectFixture{corpusFixture: newCorpusFixture(t)}
	f.seedAuthIdentity(t)
	runID := domain.RunID("review-subject")
	impl := domain.InvocationID("inv-impl-" + string(runID))
	remediator := domain.RemediationInvocationID(runID, 1)
	f.run = domain.Run{
		ID: runID, ProjectID: "proj-1", SpecDigest: subjectDigest("a"), PolicyDigest: subjectDigest("b"),
		Stages: []domain.Stage{
			{ID: corpusStageID(runID), RunID: runID, Name: "implementation", Attempts: []domain.Attempt{corpusAttempt(runID, impl)}},
			{ID: domain.RemediationStageID(runID, 1), RunID: runID, Name: "implementation", Attempts: []domain.Attempt{{
				ID: domain.AttemptID("attempt-" + string(remediator)), StageID: domain.RemediationStageID(runID, 1),
				Number: 1, InvocationID: remediator,
			}}},
		},
	}
	f.finding = domain.Finding{ID: "finding-note", RunID: runID, Source: "codex_local", Severity: domain.FindingSeverityP2, Message: "Add the decision note", RawText: "Reviewer claim", CreatedAt: f.at}
	f.round1 = domain.ReviewRecord{
		InvocationID: "review-subject-1", RunID: runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: subjectDigest("c"), InstructionDigest: subjectDigest("d"),
		CostOwner: "owner", BaseSHA: corpusBaseSHA, HeadSHA: subjectImplHead, CompletedAt: f.at.Add(time.Minute),
		CompletionEvidence: subjectDigest("e"), Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{f.finding.ID},
	}
	compatibility := domain.CompatibilityAllowed
	entry, err := domain.NewEngineAdjudicationEntry(
		f.finding.ID, domain.GoalRequired, &compatibility, domain.RouteRemediate,
		"in declared scope", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("new entry: %v", err)
	}
	f.adjudicate, err = domain.NewFindingAdjudication(runID, 1, f.run.SpecDigest, f.round1.InstructionDigest, f.run.PolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, "", f.at.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("new adjudication: %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.PutDevice(ctx, domain.Device{ID: "device-1", DisplayName: "phone", Status: domain.DeviceActive, PairedAt: f.at})
	})
	f.mustWriteInternal(t, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, string(impl), string(domain.ProductionInvocationRequestedKind),
			productionIntentPayload(impl, runID, corpusStageID(runID)))
		return err
	})
	f.seedProducer(t, corpusStageID(runID), corpusAttempt(runID, impl).ID, impl, subjectImplHead)
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, f.round1, []domain.Finding{f.finding}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, f.adjudicate)
	})
	return f
}

// seedProducer admits one attempt under the run's own spec and policy and
// records the head it exported.
func (f reviewSubjectFixture) seedProducer(t *testing.T, stageID domain.StageID, attemptID domain.AttemptID, invocation domain.InvocationID, head string) {
	t.Helper()
	identityID := domain.AuthIdentityID("auth-1")
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocation, RunID: f.run.ID, StageID: stageID, AttemptID: attemptID,
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     f.run.SpecDigest, PolicyDigest: f.run.PolicyDigest, InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: corpusRepositoryID, BaseRef: "refs/heads/main", BaseSHA: corpusBaseSHA,
		},
		Workspace: "ws-" + string(invocation), AuthIdentityID: &identityID, AdmittedAt: f.at,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocation, AdmissionID: admission.ID, ObservedBaseSHA: corpusBaseSHA, HeadSHA: head,
		ManifestDigest: "sha256:manifest", RecordedAt: f.at,
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionAdmission(context.Background(), admission); err != nil {
			return err
		}
		return tx.RecordExecutionExportRecord(context.Background(), export)
	})
}

// adjudicationItem is the round's revision-1 adjudication item, offering the
// recommended remediate route.
func (f reviewSubjectFixture) adjudicationItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	entry := f.adjudicate.Entries[0]
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionFindingAdjudicationItemID(f.run.ID, 1, 1), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(f.run.ID), RunID: &f.run.ID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason:            "choose a route for the review findings",
		RequestedDecision: []domain.Action{domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop},
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			RunID: f.run.ID, Round: 1, AdjudicationDigest: f.adjudicate.Digest,
			Proposals: []domain.FindingAdjudicationProposal{{
				FindingID: entry.FindingID, FindingMessage: domain.NormalizeFindingMessage(f.finding.Message),
				Producer: entry.Producer, GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
				Route: entry.Route, Rationale: entry.Rationale, CitedRules: entry.CitedRules,
				Assumptions: entry.Assumptions, OpenQuestions: entry.OpenQuestions, Confidence: entry.Confidence,
				OfferedAlternatives: entry.OfferedAlternatives,
			}},
		},
		PRHeadSHA: subjectImplHead, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("new item: %v", err)
	}
	ctx := context.Background()
	f.mustWrite(t, func(tx *store.WriteTx) error { return storetest.BindSubject(ctx, tx, &item) })
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatalf("put item: %v", err)
	}
	return item
}

// decideRemediation accepts the recommended remediate route and dispatches the
// remediator the way the engine does, bound to review round 1.
func (f reviewSubjectFixture) decideRemediation(t *testing.T, mutate func(*domain.RemediationInvocationIntent)) {
	t.Helper()
	ctx := context.Background()
	item := f.adjudicationItem(t)
	if _, err := f.service.Submit(ctx, commandOn(item, "command-accept-remediate", domain.ActionAcceptRecommendedRoute)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	intent := domain.RemediationInvocationIntent{
		Version: "freeside.remediation-request/v1", InvocationID: domain.RemediationInvocationID(f.run.ID, 1),
		RunID: f.run.ID, StageID: domain.RemediationStageID(f.run.ID, 1), Round: 1,
		ReviewInvocationID: f.round1.InvocationID, AdjudicationDigest: f.adjudicate.Digest,
		InputArtifactID: "remediation-input-1-" + domain.ArtifactID(f.run.ID), InputArtifactDigest: subjectDigest("f"),
		BaseSHA: f.round1.BaseSHA, HeadSHA: f.round1.HeadSHA, FindingIDs: []domain.FindingID{f.finding.ID},
	}
	if mutate != nil {
		mutate(&intent)
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	f.mustWriteInternal(t, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, string(intent.InvocationID), string(domain.RemediationInvocationRequestedKind), payload)
		return err
	})
}

func (f reviewSubjectFixture) rounds(t *testing.T) []signet.RunReviewRound {
	t.Helper()
	timeline, err := f.service.GetRunTimeline(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	return timeline.Review.Rounds
}

func TestRunReviewSubjectAndRemediation(t *testing.T) {
	ctx := context.Background()
	f := newReviewSubjectFixture(t)
	if rounds := f.rounds(t); rounds[0].Subject == nil || rounds[0].Subject.Kind != domain.ReviewSubjectImplementation ||
		rounds[0].Subject.RemediatesRound != nil || rounds[0].Remediation != nil {
		t.Fatalf("round 1 before adjudication: %#v", rounds[0])
	}
	f.decideRemediation(t, nil)
	remediator := domain.RemediationInvocationID(f.run.ID, 1)
	f.seedProducer(t, domain.RemediationStageID(f.run.ID, 1), f.run.Stages[1].Attempts[0].ID, remediator, subjectRemediationHead)
	request := domain.ReviewRequestRecord{InvocationID: "review-subject-2", RunID: f.run.ID, Round: 2, BaseSHA: corpusBaseSHA, HeadSHA: subjectRemediationHead, RequestedAt: f.at.Add(time.Hour)}
	f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutReviewRequest(ctx, request) })
	f.observe(t, domain.InvocationObservation{InvocationID: request.InvocationID, RunID: f.run.ID, Status: domain.ObservedStatusRunning, ObservedAt: request.RequestedAt})

	timeline, err := f.service.GetRunTimeline(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, second := timeline.Review.Rounds[0], timeline.Review.Rounds[1]
	if first.Remediation == nil || first.Remediation.InvocationID != remediator ||
		!slices.Equal(first.Remediation.FindingIDs, []domain.FindingID{f.finding.ID}) || first.Remediation.DecidedAt == nil {
		t.Fatalf("round 1 remediation: %#v", first.Remediation)
	}
	if second.Subject == nil || second.Subject.Kind != domain.ReviewSubjectRemediation || second.Subject.InvocationID != remediator ||
		second.Subject.RemediatesRound == nil || *second.Subject.RemediatesRound != 1 || second.Remediation != nil ||
		second.State != domain.ReviewRunning {
		t.Fatalf("round 2 subject: %#v", second)
	}
	body, err := json.MarshalIndent(timeline.Review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "run-timeline-review-remediation", body)
}

func TestRunReviewRemediationNullCases(t *testing.T) {
	t.Run("declined adjudication dispatches no remediation", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		item := f.adjudicationItem(t)
		if _, err := f.service.Submit(context.Background(), commandOn(item, "command-stop", domain.ActionStop)); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if rounds := f.rounds(t); rounds[0].Remediation != nil {
			t.Fatalf("remediation without an intent: %#v", rounds[0].Remediation)
		}
	})
	t.Run("intent bound to another head", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		f.decideRemediation(t, func(intent *domain.RemediationInvocationIntent) { intent.HeadSHA = "other-head" })
		if rounds := f.rounds(t); rounds[0].Remediation != nil {
			t.Fatalf("remediation of a head the round did not review: %#v", rounds[0].Remediation)
		}
	})
	t.Run("open adjudication has no decision time", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		f.adjudicationItem(t)
		payload, err := json.Marshal(domain.RemediationInvocationIntent{
			Version: "freeside.remediation-request/v1", InvocationID: domain.RemediationInvocationID(f.run.ID, 1),
			RunID: f.run.ID, StageID: domain.RemediationStageID(f.run.ID, 1), Round: 1,
			ReviewInvocationID: f.round1.InvocationID, AdjudicationDigest: f.adjudicate.Digest,
			InputArtifactID: "remediation-input", InputArtifactDigest: subjectDigest("f"),
			BaseSHA: f.round1.BaseSHA, HeadSHA: f.round1.HeadSHA, FindingIDs: []domain.FindingID{f.finding.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		f.mustWriteInternal(t, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(context.Background(), string(domain.RemediationInvocationID(f.run.ID, 1)), string(domain.RemediationInvocationRequestedKind), payload)
			return err
		})
		if rounds := f.rounds(t); rounds[0].Remediation == nil || rounds[0].Remediation.DecidedAt != nil {
			t.Fatalf("open adjudication: %#v", rounds[0].Remediation)
		}
	})
	t.Run("intent under the remediation key with another kind fails closed", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		id := domain.RemediationInvocationID(f.run.ID, 1)
		f.mustWriteInternal(t, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(context.Background(), string(id), string(domain.ProductionInvocationRequestedKind),
				productionIntentPayload(id, f.run.ID, domain.RemediationStageID(f.run.ID, 1)))
			return err
		})
		if _, err := f.service.GetRunTimeline(context.Background(), f.run.ID); err == nil {
			t.Fatal("foreign intent kind accepted as a remediation")
		}
	})
}

// TestRunReviewRemediationLineageFailsClosed pins the re-gate of a decoded
// remediation intent: dispatch authentication proves only its identity, so a
// finding or review lineage the round's records do not carry fails the read.
func TestRunReviewRemediationLineageFailsClosed(t *testing.T) {
	t.Run("remediation routes a finding the round did not report", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		f.decideRemediation(t, func(intent *domain.RemediationInvocationIntent) {
			intent.FindingIDs = []domain.FindingID{f.finding.ID, "finding-not-reported"}
		})
		if _, err := f.service.GetRunTimeline(context.Background(), f.run.ID); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("unreported finding accepted: %v", err)
		}
	})
	t.Run("remediation names another adjudication", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		f.decideRemediation(t, func(intent *domain.RemediationInvocationIntent) { intent.AdjudicationDigest = subjectDigest("e") })
		if _, err := f.service.GetRunTimeline(context.Background(), f.run.ID); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("unbound adjudication accepted: %v", err)
		}
	})
	t.Run("remediator subject bound to a head its round did not review", func(t *testing.T) {
		ctx := context.Background()
		f := newReviewSubjectFixture(t)
		f.decideRemediation(t, func(intent *domain.RemediationInvocationIntent) { intent.HeadSHA = "other-head" })
		remediator := domain.RemediationInvocationID(f.run.ID, 1)
		f.seedProducer(t, domain.RemediationStageID(f.run.ID, 1), f.run.Stages[1].Attempts[0].ID, remediator, subjectRemediationHead)
		request := domain.ReviewRequestRecord{InvocationID: "review-subject-2", RunID: f.run.ID, Round: 2, BaseSHA: corpusBaseSHA, HeadSHA: subjectRemediationHead, RequestedAt: f.at.Add(time.Hour)}
		f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutReviewRequest(ctx, request) })
		if _, err := f.service.GetRunTimeline(ctx, f.run.ID); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("remediator subject with foreign lineage accepted: %v", err)
		}
	})
}

func TestRunReviewSubjectNullCases(t *testing.T) {
	ctx := context.Background()
	t.Run("failed review retried on the same head keeps its producer", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		request := domain.ReviewRequestRecord{InvocationID: "review-subject-2", RunID: f.run.ID, Round: 2, BaseSHA: corpusBaseSHA, HeadSHA: subjectImplHead, RequestedAt: f.at.Add(time.Hour)}
		f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutReviewRequest(ctx, request) })
		if rounds := f.rounds(t); rounds[1].Subject == nil || rounds[1].Subject.Kind != domain.ReviewSubjectImplementation {
			t.Fatalf("retry subject: %#v", rounds[1].Subject)
		}
	})
	t.Run("head with no recorded producer", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		request := domain.ReviewRequestRecord{InvocationID: "review-subject-2", RunID: f.run.ID, Round: 2, BaseSHA: corpusBaseSHA, HeadSHA: "successor-head", RequestedAt: f.at.Add(time.Hour)}
		f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutReviewRequest(ctx, request) })
		if rounds := f.rounds(t); rounds[1].Subject != nil {
			t.Fatalf("unproven producer claimed: %#v", rounds[1].Subject)
		}
	})
	t.Run("head exported twice", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		f.decideRemediation(t, nil)
		f.seedProducer(t, domain.RemediationStageID(f.run.ID, 1), f.run.Stages[1].Attempts[0].ID, domain.RemediationInvocationID(f.run.ID, 1), subjectImplHead)
		if rounds := f.rounds(t); rounds[0].Subject != nil {
			t.Fatalf("ambiguous producer claimed: %#v", rounds[0].Subject)
		}
	})
	t.Run("producer with no retained intent", func(t *testing.T) {
		f := newReviewSubjectFixture(t)
		db, err := sql.Open("sqlite", f.path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := db.ExecContext(ctx, "DELETE FROM outbox WHERE kind = ?", string(domain.ProductionInvocationRequestedKind)); err != nil {
			t.Fatal(err)
		}
		if rounds := f.rounds(t); rounds[0].Subject != nil {
			t.Fatalf("legacy producer claimed: %#v", rounds[0].Subject)
		}
	})
}
