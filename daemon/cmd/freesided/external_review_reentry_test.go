package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	externalTestMaintainerID    = 4101
	externalTestMaintainerLogin = "maintainer"
)

func externalTestMaintainer() domain.ExternalReviewer {
	return domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: externalTestMaintainerID,
		Login: externalTestMaintainerLogin, Authority: domain.ExternalReviewDriveRound,
	}
}

// activateExternalTestProfile records and activates a trust profile for the
// bound repository whose allowlist is reviewers. The digest differs by
// allowlist, so a test can activate one profile after another.
func activateExternalTestProfile(
	t *testing.T, st *store.Store, at time.Time, reviewers ...domain.ExternalReviewer,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "owner/repo", RepositoryID: 424242,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config"},
		ExternalReviewers:          reviewers,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, at); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, "owner/repo", profile.ProfileDigest, at)
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

// externalReentryActivity is two inline comments by the maintainer on the
// published head, the second a minute after the first, and one by a bot no
// test lists.
func externalReentryActivity() publish.PullReviewObservation {
	comment := func(id, author int64, login, body string, at time.Time) publish.PullReviewComment {
		return publish.PullReviewComment{
			ID: id, AuthorID: author, AuthorLogin: login,
			Path: "daemon/main.go", Line: 42, OriginalLine: 42, Body: body,
			CommitID: reentryTestHead, OriginalCommitID: reentryTestHead, CreatedAt: at,
		}
	}
	return publish.PullReviewObservation{Comments: []publish.PullReviewComment{
		comment(800401, externalTestMaintainerID, externalTestMaintainerLogin,
			"second: also this", activeResourceTestTime.Add(time.Minute)),
		comment(800400, externalTestMaintainerID, externalTestMaintainerLogin,
			"first: this leaks the handle", activeResourceTestTime),
		comment(800402, 4102, "review-bot[bot]", "nit: rename", activeResourceTestTime.Add(-time.Minute)),
	}}
}

// externalReentryReconciler reconciles the open pull request reentryReadyItem
// binds, at its bound head, with review observing its activity.
func externalReentryReconciler(st *store.Store, review nativeReviewObserver) activeResourceReconciler {
	return activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.HeadSHA = reentryTestHead
			return observed, nil
		},
		review:    review,
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
}

func reconcileClean(t *testing.T, reconciler *activeResourceReconciler, step string) {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background())
	if err != nil || len(result.failures) != 0 || len(result.skipped) != 0 {
		t.Fatalf("%s: Reconcile = %+v, %v", step, result, err)
	}
}

// externalFindingByThread returns the finding the fixture's activity builds
// for one thread under the binding of reentryReadyItem's run.
func externalFindingByThread(t *testing.T, runID domain.RunID, thread string) domain.Finding {
	t.Helper()
	built, skipped := buildExternalFindings(externalReentryActivity(), domain.ReadyItemPRBinding{
		RunID: runID, Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
		BaseRef: "main", HeadSHA: reentryTestHead,
	})
	if len(skipped) != 0 {
		t.Fatalf("fixture skipped activity: %v", skipped)
	}
	for _, finding := range built {
		if finding.External.ThreadID == thread {
			return finding
		}
	}
	t.Fatalf("fixture builds no finding for thread %s", thread)
	return domain.Finding{}
}

func assertExternalReviewStarted(
	t *testing.T, st *store.Store, item domain.AttentionItem,
	profile domain.AutomationTrustProfile, trigger domain.FindingID,
) {
	t.Helper()
	ctx := context.Background()
	runID := *item.Subject.RunID
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusSuperseded || got.ItemVersion != item.ItemVersion+1 ||
		got.ReadinessInvalidation != nil {
		t.Fatalf("ready item = status %s v%d invalidation %+v; want superseded, one version on, no invalidation",
			got.Status, got.ItemVersion, got.ReadinessInvalidation)
	}
	successor, queued := readReentry(t, st, runID)
	if successor == nil || !queued {
		t.Fatalf("cycle = authority %+v, task queued %t; want both", successor, queued)
	}
	if successor.Version != domain.PublicationExternalReviewVersion ||
		successor.Origin != domain.PublicationSuccessorExternalReview ||
		successor.PredecessorItemID != item.ID ||
		successor.PriorReviewInvocationID != reentryTestReviewID || successor.ReviewRound != 2 ||
		successor.Reentry == nil || successor.Reentry.Reason != "" ||
		successor.Reentry.BaseSHA != reentryTestBase || successor.Reentry.HeadSHA != reentryTestHead ||
		successor.ExternalFindingID != trigger ||
		successor.AdmittingProfileDigest != profile.ProfileDigest {
		t.Fatalf("authority = %+v (re-entry %+v); want an external review cycle for finding %s under profile %s",
			successor, successor.Reentry, trigger, profile.ProfileDigest)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, runID)
		if err != nil {
			return err
		}
		if len(chain) != 1 {
			t.Errorf("successor chain = %d authorities, want 1", len(chain))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); !schedule.Status.Terminal() {
			t.Errorf("schedule %s not concluded: %s", id, schedule.Status)
		}
	}
}

// assertNoExternalReview proves a pass that starts nothing leaves the ready
// item exactly as it was.
func assertNoExternalReview(t *testing.T, st *store.Store, item domain.AttentionItem) {
	t.Helper()
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("ready item = status %s v%d; want it open at v%d", got.Status, got.ItemVersion, item.ItemVersion)
	}
	assertNoReentry(t, st, *item.Subject.RunID)
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); schedule.Status.Terminal() {
			t.Errorf("schedule %s concluded with no cycle started: %s", id, schedule.Status)
		}
	}
}

// TestActiveResourceStartsExternalReviewCycle proves an admitted reviewer's
// finding on the published head supersedes the ready item with no
// invalidation, seals an external review authority naming the earliest
// admitted finding and the profile that admitted it, queues the cycle's task,
// and concludes the item's watches. Two findings on one head start one cycle,
// and a later pass starts no second one (issue #524, decision 5).
func TestActiveResourceStartsExternalReviewCycle(t *testing.T) {
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-external-start")
	armActiveTestSchedules(t, st, item)
	profile := activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler := externalReentryReconciler(st, staticReview(externalReentryActivity()))

	reconcileClean(t, &reconciler, "first pass")
	earliest := externalFindingByThread(t, *item.Subject.RunID, "review_comment/800400")
	assertExternalReviewStarted(t, st, item, profile, earliest.ID)

	started := readRevision(t, st)
	reconcileClean(t, &reconciler, "second pass")
	assertExternalReviewStarted(t, st, item, profile, earliest.ID)
	if got := readRevision(t, st); got != started {
		t.Fatalf("a pass after the cycle started moved the revision %d to %d", started, got)
	}
}

// TestActiveResourceExternalReviewStartsNothing proves each condition under
// which no cycle may start leaves the ready item open at its version with its
// watches armed, seals nothing, queues nothing, and costs no write: a second
// pass over the same state leaves the client-visible revision alone.
func TestActiveResourceExternalReviewStartsNothing(t *testing.T) {
	before := activeResourceTestTime.Add(-time.Hour)
	otherHead := strings.Repeat("e", 40)
	for _, tc := range []struct {
		name     string
		arrange  func(t *testing.T, st *store.Store, runID domain.RunID)
		activity func(*publish.PullReviewObservation)
	}{
		{name: "no trust profile"},
		{
			name: "reviewer not listed",
			arrange: func(t *testing.T, st *store.Store, _ domain.RunID) {
				other := externalTestMaintainer()
				other.AccountID, other.Login = 5000, "someone-else"
				activateExternalTestProfile(t, st, before, other)
			},
		},
		{
			name: "listed account under another login",
			arrange: func(t *testing.T, st *store.Store, _ domain.RunID) {
				renamed := externalTestMaintainer()
				renamed.Login = "maintainer-old"
				activateExternalTestProfile(t, st, before, renamed)
			},
		},
		{
			name: "listed login under another account",
			arrange: func(t *testing.T, st *store.Store, _ domain.RunID) {
				reused := externalTestMaintainer()
				reused.AccountID = 5001
				activateExternalTestProfile(t, st, before, reused)
			},
		},
		{
			name: "reviewer removed from the profile",
			arrange: func(t *testing.T, st *store.Store, _ domain.RunID) {
				activateExternalTestProfile(t, st, before.Add(-time.Hour), externalTestMaintainer())
				activateExternalTestProfile(t, st, before)
			},
		},
		{
			name: "finding on another head",
			arrange: func(t *testing.T, st *store.Store, _ domain.RunID) {
				activateExternalTestProfile(t, st, before, externalTestMaintainer())
			},
			activity: func(obs *publish.PullReviewObservation) {
				for i := range obs.Comments {
					obs.Comments[i].OriginalCommitID = otherHead
				}
			},
		},
		{
			name: "latest review covers another head",
			arrange: func(t *testing.T, st *store.Store, runID domain.RunID) {
				activateExternalTestProfile(t, st, before, externalTestMaintainer())
				review, err := domain.NewReviewRecord(domain.ReviewRecord{
					InvocationID: "review-external-round-2", RunID: runID, Round: 2,
					Provider: "openai", ModelConfiguration: "gpt-codex/high",
					ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
					InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
					CostOwner:           "owner", BaseSHA: reentryTestBase, HeadSHA: otherHead,
					CompletedAt:        activeResourceTestTime.Add(-time.Minute),
					CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("f", 64)),
					Outcome:            domain.ReviewClean,
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					return tx.PutReviewRecord(ctx, review, nil)
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := schedTestStore(t)
			item := reentryReadyItem(t, st, "run-external-none")
			armActiveTestSchedules(t, st, item)
			if tc.arrange != nil {
				tc.arrange(t, st, *item.Subject.RunID)
			}
			activity := externalReentryActivity()
			if tc.activity != nil {
				tc.activity(&activity)
			}
			reconciler := externalReentryReconciler(st, staticReview(activity))
			reconcileClean(t, &reconciler, "first pass")
			assertNoExternalReview(t, st, item)
			settled := readRevision(t, st)
			reconcileClean(t, &reconciler, "second pass")
			assertNoExternalReview(t, st, item)
			if got := readRevision(t, st); got != settled {
				t.Fatalf("a pass that starts nothing moved the revision %d to %d", settled, got)
			}
		})
	}
}

// TestActiveResourceExternalReviewStartsAfterListing proves the trigger reads
// stored findings, not the pass's fetch: activity stored while its reviewer
// was unlisted starts a cycle on the first pass after the owner lists them,
// though the forge then answers "not modified".
func TestActiveResourceExternalReviewStartsAfterListing(t *testing.T) {
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-external-listed")
	armActiveTestSchedules(t, st, item)
	fetched := false
	reconciler := externalReentryReconciler(st,
		func(context.Context, string, int) (publish.PullReviewObservation, error) {
			if fetched {
				return publish.PullReviewObservation{NotModified: true}, nil
			}
			fetched = true
			return externalReentryActivity(), nil
		})

	reconcileClean(t, &reconciler, "unlisted pass")
	assertNoExternalReview(t, st, item)

	profile := activateExternalTestProfile(t, st, activeResourceTestTime, externalTestMaintainer())
	reconcileClean(t, &reconciler, "listed pass")
	earliest := externalFindingByThread(t, *item.Subject.RunID, "review_comment/800400")
	assertExternalReviewStarted(t, st, item, profile, earliest.ID)
}

// TestActiveResourceExternalReviewNeedsAnOpenPull proves a stored, admitted
// finding starts nothing once the pull request is no longer open. The case is
// a merged pull request whose bound issue has not closed yet: the ready item
// is still open and its watches armed, so only the pull request's own state
// keeps a cycle from starting on a head that already merged.
func TestActiveResourceExternalReviewNeedsAnOpenPull(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-external-merged")
	runID := *item.Subject.RunID
	armActiveTestSchedules(t, st, item)
	boundIssue := 443
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR, BoundIssue: &boundIssue,
	}, runID, "project-1", activeResourceTestTime.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, domain.WorkUnitPRBinding{
			UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: reentryTestHead,
			RecordedAt: activeResourceTestTime.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}

	merged := false
	reconciler := externalReentryReconciler(st, staticReview(externalReentryActivity()))
	reconciler.pull = func(context.Context, string, int) (publish.PullObservation, error) {
		observed := exactPull("open", false)
		if merged {
			observed = exactPull("closed", true)
		}
		observed.HeadSHA = reentryTestHead
		return observed, nil
	}
	reconciler.issue = func(context.Context, string, int) (publish.IssueObservation, error) {
		return publish.IssueObservation{Number: boundIssue, State: "open"}, nil
	}
	reconcileClean(t, &reconciler, "unlisted pass")
	assertNoExternalReview(t, st, item)

	activateExternalTestProfile(t, st, activeResourceTestTime, externalTestMaintainer())
	merged = true
	reconcileClean(t, &reconciler, "merged pass")
	assertNoExternalReview(t, st, item)
}

// TestActiveResourceExternalReviewStartFailureIsolated proves a cycle that
// cannot be recorded changes nothing and blocks nothing: the supersession
// rolls back with the authority, the pass's pull fact is committed, the
// failure is collected, and the next pass starts the cycle.
func TestActiveResourceExternalReviewStartFailureIsolated(t *testing.T) {
	// A store at a path the test can also reach through a second connection.
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	item := reentryReadyItem(t, st, "run-external-failure")
	armActiveTestSchedules(t, st, item)
	profile := activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	// The task is the trigger's last write, after the supersession and the
	// authority, so refusing it proves both roll back.
	execStoreSQL(t, dbPath, `CREATE TRIGGER refuse_successors BEFORE INSERT ON outbox
		WHEN NEW.kind = 'production_publication_requested'
		BEGIN SELECT RAISE(ABORT, 'cycle tasks are refused'); END`)
	reconciler := externalReentryReconciler(st, staticReview(externalReentryActivity()))

	result, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(result.failures) != 1 || !strings.Contains(result.failures[0].Error(), "start external review") {
		t.Fatalf("failures = %v, want one isolated start failure", result.failures)
	}
	assertNoExternalReview(t, st, item)
	pulls := 0
	for _, fact := range readPullTimeline(t, st) {
		if fact.PRNumber == 450 && fact.HeadSHA == reentryTestHead {
			pulls++
		}
	}
	if pulls != 1 {
		t.Fatalf("pull facts for the bound head = %d, want the pass's fact committed despite the failure", pulls)
	}

	execStoreSQL(t, dbPath, "DROP TRIGGER refuse_successors")
	reconcileClean(t, &reconciler, "retry")
	earliest := externalFindingByThread(t, *item.Subject.RunID, "review_comment/800400")
	assertExternalReviewStarted(t, st, item, profile, earliest.ID)
}

// TestExternalReviewReentryRefusesEndedWork proves the trigger starts nothing
// for a run whose work has ended: a task that is being stopped, and a unit
// whose completion is recorded. Each is decided from reads, so the probe and
// the writer agree and the ready item is left as it was. The control case
// proves the same fixture starts a cycle when neither holds.
func TestExternalReviewReentryRefusesEndedWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, st *store.Store, runID domain.RunID)
		due     bool
	}{
		{name: "control", due: true},
		{
			name: "task stopping",
			arrange: func(t *testing.T, st *store.Store, runID domain.RunID) {
				ctx := context.Background()
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					run, err := tx.GetRun(ctx, runID)
					if err != nil {
						return err
					}
					state, err := tx.ServerState(ctx)
					if err != nil {
						return err
					}
					_, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
						CommandID: "stop-external", DeviceID: "device", TaskID: run.TaskID,
						ProjectID: run.ProjectID, ExpectedSyncEpoch: state.SyncEpoch,
						ExpectedEntityVersion: state.Revision,
					}, activeResourceTestTime)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "publication complete",
			arrange: func(t *testing.T, st *store.Store, runID domain.RunID) {
				ctx := context.Background()
				declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
					CompletionCriterion: domain.CompletionBoundPRMerged,
				}, runID, "project-1", activeResourceTestTime.Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				binding := domain.WorkUnitPRBinding{
					UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
					PRNumber: 450, BaseRef: "main", HeadSHA: reentryTestHead,
					RecordedAt: activeResourceTestTime.Add(-time.Minute),
				}
				pull := domain.PullMergeFact{
					Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
					State: domain.PullRequestClosed, Merged: true, MergeCommitSHA: "deadbeef",
					BaseRef: "main", HeadSHA: reentryTestHead, ObservedAt: activeResourceTestTime.Add(time.Minute),
				}
				completion, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, nil)
				if !ok {
					t.Fatal("fixture did not derive completion")
				}
				if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
					if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
						return err
					}
					return tx.RecordWorkUnitPRBinding(ctx, binding)
				}); err != nil {
					t.Fatal(err)
				}
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					if _, err := tx.AppendPullMergeFact(ctx, pull); err != nil {
						return err
					}
					return tx.RecordWorkUnitCompletion(ctx, completion)
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := reentryReadyItem(t, st, "run-external-ended")
			armActiveTestSchedules(t, st, item)
			// Store the findings while nobody is listed, so nothing starts yet.
			reconciler := externalReentryReconciler(st, staticReview(externalReentryActivity()))
			reconcileClean(t, &reconciler, "unlisted pass")
			assertNoExternalReview(t, st, item)
			activateExternalTestProfile(t, st, activeResourceTestTime, externalTestMaintainer())
			if tc.arrange != nil {
				tc.arrange(t, st, *item.Subject.RunID)
			}

			var due bool
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				due, err = engine.ExternalReviewReentryDue(ctx, tx, item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var started bool
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				var err error
				started, err = engine.StartExternalReviewReentry(ctx, tx, item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if due != tc.due || started != tc.due {
				t.Fatalf("due = %t, started = %t; want both %t", due, started, tc.due)
			}
			if !tc.due {
				assertNoExternalReview(t, st, item)
			}
		})
	}
}
