package signet_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// corpusCompletionBoundIssue is the issue the completed-run fixture binds,
// so the bound-issue criterion (and the bound_issue wire fact) is exercised.
const corpusCompletionBoundIssue = 443

// seedPublishedRun seeds a run through publication: the attempt and its
// admission and export, the ready item with its binding, and the resolved
// policy the work-unit declaration re-gate re-derives the declared scope
// from. The run's policy digest is the policy's content digest, so the
// admission must carry it too. No milestone is appended.
func (f corpusFixture) seedPublishedRun(
	t *testing.T, runID domain.RunID, attemptInvocation domain.InvocationID,
) domain.ResolvedPolicy {
	t.Helper()
	ctx := context.Background()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/,devlog/",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		},
	}})
	if err != nil {
		t.Fatalf("NewResolvedPolicy: %v", err)
	}
	run := corpusRun(runID, corpusAttempt(runID, attemptInvocation))
	run.PolicyDigest = policy.Digest
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	})
	admission := f.seedAdmissionWithPolicy(
		t, runID, corpusStageID(runID), corpusAttempt(runID, attemptInvocation).ID,
		attemptInvocation, policy.Digest,
	)
	f.seedExport(t, admission)
	item, err := f.readyItem(runID)
	if err != nil {
		t.Fatalf("readyItem: %v", err)
	}
	f.seedItem(t, item)
	f.seedReadyBinding(t, runID, attemptInvocation, publicationInvocation(runID))
	return policy
}

// seedCompletionRecords records the work unit behind a published run and its
// completion evidence exactly as the capture pass does: the declaration, the
// PR binding matching the ready binding, the merged pull fact, the closed
// issue fact, and the evaluated completion row. It returns the recorded
// completion; no milestone is appended.
func (f corpusFixture) seedCompletionRecords(
	t *testing.T, runID domain.RunID, policy domain.ResolvedPolicy,
) domain.WorkUnitCompletion {
	t.Helper()
	return f.seedCompletionRecordsForPR(t, runID, policy, corpusReadyPRNumber)
}

// corpusReadyPRNumber is the pull request the corpus run publishes, so the
// seeded ready resource binding names it. A completion seeded for any other
// number is the internally consistent forgery the read-side binding check
// exists to refuse.
const corpusReadyPRNumber = 123

// seedCompletionRecordsForPR is seedCompletionRecords over a chosen pull
// request, so a forge can build a completion set that agrees with itself and
// disagrees with the run's published resource.
func (f corpusFixture) seedCompletionRecordsForPR(
	t *testing.T, runID domain.RunID, policy domain.ResolvedPolicy, prNumber int,
) domain.WorkUnitCompletion {
	t.Helper()
	ctx := context.Background()
	boundIssue := corpusCompletionBoundIssue
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, runID, "proj-1", f.at)
	if err != nil {
		t.Fatalf("NewWorkUnitDeclaration: %v", err)
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: corpusRepositoryID,
		PRNumber: prNumber, BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		RecordedAt: f.at.Add(time.Hour),
	}
	pull := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: corpusRepositoryID, PRNumber: prNumber,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "feedface", BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		ObservedAt: f.at.Add(4 * time.Hour),
	}
	issue := domain.IssueStateFact{
		Repo: "owner/repo", RepositoryID: corpusRepositoryID, IssueNumber: boundIssue,
		State: domain.IssueClosed, ClosedByCommitSHA: "feedface",
		ObservedAt: f.at.Add(4 * time.Hour),
	}
	completion, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, &issue)
	if !ok {
		t.Fatal("completion fixture did not evaluate as completed")
	}
	f.mustWriteInternal(t, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitPRBinding(ctx, binding); err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(ctx, issue); err != nil {
			return err
		}
		return tx.RecordWorkUnitCompletion(ctx, completion)
	})
	return completion
}

func (f corpusFixture) appendPublicationReady(t *testing.T, runID domain.RunID) {
	t.Helper()
	f.appendMilestone(t, domain.RunMilestone{
		RunID: runID, Kind: domain.MilestonePublicationReady,
		InvocationID: ptr(publicationInvocation(runID)), RecordedAt: f.at.Add(3 * time.Hour),
	})
}

func (f corpusFixture) appendWorkUnitCompleted(t *testing.T, runID domain.RunID, at time.Time) {
	t.Helper()
	f.appendMilestone(t, domain.RunMilestone{
		RunID: runID, Kind: domain.MilestoneWorkUnitCompleted,
		InvocationID: ptr(publicationInvocation(runID)), RecordedAt: at,
	})
}

// TestRunObservationCompletedBaseline is the valid work_unit_completed
// baseline (#1134): the timeline ends with the completion, the run reads as
// completed and finished, and the summary and timeline carry the completion
// facts read from the re-gated record, not from the milestone.
func TestRunObservationCompletedBaseline(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	runID := domain.RunID("run-completed")
	policy := f.seedPublishedRun(t, runID, "inv-completed-attempt")
	completion := f.seedCompletionRecords(t, runID, policy)
	f.appendPublicationReady(t, runID)
	f.appendWorkUnitCompleted(t, runID, completion.RecordedAt)
	if err := f.read(ctx, runID); err != nil {
		t.Fatalf("valid work_unit_completed baseline: %v", err)
	}

	wantFacts := signet.WorkUnitCompletionFacts{
		PRNumber: 123, MergeCommitSHA: "feedface",
		BoundIssue: ptr(corpusCompletionBoundIssue), RecordedAt: completion.RecordedAt,
	}
	assertFacts := func(t *testing.T, name string, got *signet.WorkUnitCompletionFacts) {
		t.Helper()
		if got == nil || got.BoundIssue == nil {
			t.Fatalf("%s completion = %+v, want %+v", name, got, wantFacts)
		}
		if got.PRNumber != wantFacts.PRNumber || got.MergeCommitSHA != wantFacts.MergeCommitSHA ||
			*got.BoundIssue != *wantFacts.BoundIssue || !got.RecordedAt.Equal(wantFacts.RecordedAt) {
			t.Fatalf("%s completion = %+v, want %+v", name, *got, wantFacts)
		}
	}
	run, err := f.service.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.Outcome != domain.RunOutcomeCompleted || run.Run.Lifecycle != domain.RunLifecycleFinished {
		t.Fatalf("run outcome/lifecycle = %s/%s, want completed/finished", run.Run.Outcome, run.Run.Lifecycle)
	}
	if run.Run.SupersededBy != nil {
		t.Fatalf("superseded_by = %q, want null", *run.Run.SupersededBy)
	}
	if run.Run.LatestMilestone == nil || *run.Run.LatestMilestone != domain.MilestoneWorkUnitCompleted {
		t.Fatalf("latest milestone = %v, want work_unit_completed", run.Run.LatestMilestone)
	}
	assertFacts(t, "run", run.Run.Completion)
	if run.Run.BillableCostSoFar != nil {
		t.Fatalf("billable_cost_so_far = %+v, want null before any billable observation", run.Run.BillableCostSoFar)
	}
	timeline, err := f.service.GetRunTimeline(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if last := timeline.Milestones[len(timeline.Milestones)-1]; last.Kind != domain.MilestoneWorkUnitCompleted {
		t.Fatalf("timeline ends with %s, want work_unit_completed", last.Kind)
	}
	assertFacts(t, "timeline", timeline.Completion)
	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Run.Outcome != domain.RunOutcomeCompleted {
		t.Fatalf("ListRuns = %+v, want the one completed run", runs)
	}
}

// TestRunObservationCompletionForges is the adversarial half for the
// completion milestone: a milestone with no supported completion record
// behind it, one without ready authority after the last block, one whose
// instant disagrees with the record, and one whose internally consistent
// binding names a different pull request than the run published all fail the
// read closed.
func TestRunObservationCompletionForges(t *testing.T) {
	ctx := context.Background()
	cases := []forgeCase{
		{"work_unit_completed/missing_completion_record", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.seedPublishedRun(t, runID, "inv-1")
			f.appendPublicationReady(t, runID)
			f.appendWorkUnitCompleted(t, runID, f.at.Add(4*time.Hour))
			return runID
		}},
		{"work_unit_completed/missing_publication_ready", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecords(t, runID, policy)
			f.appendWorkUnitCompleted(t, runID, completion.RecordedAt)
			return runID
		}},
		{"work_unit_completed/blocked_after_ready", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecords(t, runID, policy)
			item, err := blockedItem(runID, domain.PublicationBlockTrust,
				[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop})
			if err != nil {
				t.Fatal(err)
			}
			f.seedItem(t, item)
			f.appendPublicationReady(t, runID)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestonePublicationBlocked,
				InvocationID: ptr(publicationInvocation(runID)), Reason: ptr(domain.HoldTrustBlocked),
				RecordedAt: f.at.Add(3*time.Hour + 30*time.Minute),
			})
			f.appendWorkUnitCompleted(t, runID, completion.RecordedAt)
			return runID
		}},
		{"work_unit_completed/instant_disagrees_with_record", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecords(t, runID, policy)
			f.appendPublicationReady(t, runID)
			f.appendWorkUnitCompleted(t, runID, completion.RecordedAt.Add(time.Minute))
			return runID
		}},
		{"work_unit_completed/binding_disagrees_with_ready", func(t *testing.T, f corpusFixture) domain.RunID {
			// Declaration, binding, merge fact, and completion all agree
			// with each other on PR 456, so the store's own re-derivation
			// supports the row; only the run's ready resource disagrees.
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecordsForPR(t, runID, policy, corpusReadyPRNumber+333)
			f.appendPublicationReady(t, runID)
			f.appendWorkUnitCompleted(t, runID, completion.RecordedAt)
			return runID
		}},
		{"work_unit_completed/foreign_invocation", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecords(t, runID, policy)
			f.appendPublicationReady(t, runID)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneWorkUnitCompleted,
				InvocationID: ptr(publicationInvocation("run-other")), RecordedAt: completion.RecordedAt,
			})
			return runID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCorpusFixture(t)
			f.seedAuthIdentity(t)
			runID := tc.build(t, f)
			err := f.read(ctx, runID)
			assertFailClosed(t, tc.name, err)
			if !errors.Is(err, signet.ErrRunObservationIntegrity) {
				t.Fatalf("forge %q error = %v, want ErrRunObservationIntegrity", tc.name, err)
			}
		})
	}
}

// TestListingReadsExcludeForgedCompletion pins the listing contract for a
// forged completion: the run is excluded from Bootstrap and ListRuns, never
// shown as completed, while the healthy sibling is still served.
func TestListingReadsExcludeForgedCompletion(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := newCorpusFixture(t, signet.WithLogger(logger))
	f.seedAuthIdentity(t)
	healthy := domain.RunID("run-healthy")
	forged := domain.RunID("run-forged")
	f.seedTerminalRun(t, healthy, "inv-healthy", domain.ObservedStatusCompleted, domain.ObservedStatusCompleted)
	f.seedPublishedRun(t, forged, "inv-forged")
	f.appendPublicationReady(t, forged)
	f.appendWorkUnitCompleted(t, forged, f.at.Add(4*time.Hour))

	for name, list := range map[string]func() ([]domain.RunID, error){
		"Bootstrap": func() ([]domain.RunID, error) {
			bootstrap, err := f.service.Bootstrap(ctx)
			if err != nil {
				return nil, err
			}
			ids := make([]domain.RunID, 0, len(bootstrap.Runs))
			for _, run := range bootstrap.Runs {
				ids = append(ids, run.Run.ID)
			}
			return ids, nil
		},
		"ListRuns": func() ([]domain.RunID, error) {
			runs, err := f.service.ListRuns(ctx)
			if err != nil {
				return nil, err
			}
			ids := make([]domain.RunID, 0, len(runs))
			for _, run := range runs {
				ids = append(ids, run.Run.ID)
			}
			return ids, nil
		},
	} {
		ids, err := list()
		if err != nil {
			t.Fatalf("%s over a forged completion = %v, want the healthy run served", name, err)
		}
		if len(ids) != 1 || ids[0] != healthy {
			t.Fatalf("%s served %v, want only %q", name, ids, healthy)
		}
	}
	if out := logs.String(); !strings.Contains(out, "excluding run from listing") || !strings.Contains(out, string(forged)) {
		t.Fatalf("logs = %q, want a warn naming excluded run %q", out, forged)
	}
}

// TestAuthenticatedRunConclusionRejectsUnsupportedCompletion pins the
// direct-store conclusion surface. freesided follow and follow -snapshot
// reach AuthenticatedRunConclusion through observedb without the sync
// boundary's observation pass, so the completion binding has to live in the
// conclusion itself: a milestone whose re-gated completion record no longer
// stands fails the conclusion closed instead of reporting a final completed
// outcome the sync reads refuse over the same rows. Both of the milestone's
// authorities are checked there: the completion record and the run's own
// publication invocation.
func TestAuthenticatedRunConclusionRejectsUnsupportedCompletion(t *testing.T) {
	ctx := context.Background()
	cases := []forgeCase{
		{"no_completion_record", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			f.seedPublishedRun(t, runID, "inv-1")
			f.appendPublicationReady(t, runID)
			f.appendWorkUnitCompleted(t, runID, f.at.Add(4*time.Hour))
			return runID
		}},
		{"foreign_publication_invocation", func(t *testing.T, f corpusFixture) domain.RunID {
			runID := domain.RunID("run-1")
			policy := f.seedPublishedRun(t, runID, "inv-1")
			completion := f.seedCompletionRecords(t, runID, policy)
			f.appendPublicationReady(t, runID)
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneWorkUnitCompleted,
				InvocationID: ptr(publicationInvocation("run-other")), RecordedAt: completion.RecordedAt,
			})
			return runID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCorpusFixture(t)
			f.seedAuthIdentity(t)
			runID := tc.build(t, f)
			var conclusion domain.RunConclusion
			err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				run, err := tx.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				observation, err := tx.ObserveRun(ctx, runID)
				if err != nil {
					return err
				}
				conclusion, err = signet.AuthenticatedRunConclusion(ctx, tx, run, observation, true)
				return err
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("AuthenticatedRunConclusion() = %+v, %v, want ErrParentKeyMismatch", conclusion, err)
			}
		})
	}
}

// TestListingReadsExcludeUnreconstructableCompletion pins the same listing
// contract for the other half of the completion re-gate's verdicts. A row the
// store refuses because its evidence does not derive it and a row it cannot
// reconstruct at all are both contradictions about one run, so both exclude
// that run and serve the rest; neither fails the whole request. The row is
// damaged through a raw connection because the write boundary refuses to
// produce it.
func TestListingReadsExcludeUnreconstructableCompletion(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := newCorpusFixture(t, signet.WithLogger(logger))
	f.seedAuthIdentity(t)
	healthy := domain.RunID("run-healthy")
	damaged := domain.RunID("run-damaged")
	f.seedTerminalRun(t, healthy, "inv-healthy", domain.ObservedStatusCompleted, domain.ObservedStatusCompleted)
	policy := f.seedPublishedRun(t, damaged, "inv-damaged")
	completion := f.seedCompletionRecords(t, damaged, policy)
	f.appendPublicationReady(t, damaged)
	f.appendWorkUnitCompleted(t, damaged, completion.RecordedAt)

	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx,
		`UPDATE work_unit_completions SET body = '{"unit_id":"workunit-other"}' WHERE unit_id = ?`,
		completion.UnitID); err != nil {
		t.Fatalf("damage completion row: %v", err)
	}

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns over an unreconstructable completion = %v, want the healthy run served", err)
	}
	if len(runs) != 1 || runs[0].Run.ID != healthy {
		ids := make([]domain.RunID, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.Run.ID)
		}
		t.Fatalf("ListRuns served %v, want only %q", ids, healthy)
	}
	if out := logs.String(); !strings.Contains(out, "excluding run from listing") || !strings.Contains(out, string(damaged)) {
		t.Fatalf("logs = %q, want a warn naming excluded run %q", out, damaged)
	}
}
