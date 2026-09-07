package signet_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
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
