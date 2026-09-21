package publish_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func stopPublicationTask(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if err != nil {
			return err
		}
		state, err := tx.ServerState(t.Context())
		if err != nil {
			return err
		}
		_, _, err = tx.StopTask(t.Context(), domain.StopTaskRequest{CommandID: "publication-stop", DeviceID: "device", TaskID: run.TaskID, ProjectID: run.ProjectID, ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCancellationPublicationBoundaryOrder(t *testing.T) {
	for _, boundary := range []string{"before-intent", "after-intent", "before-pr", "in-flight-pr"} {
		t.Run(boundary, func(t *testing.T) {
			head := testHeadSHA
			st := newExecutionBoundStore(t, executionChainOptions{})
			seedDecisionRecords(t, st)
			reservation := seedExecutionPublicationChain(t, st, executionChainOptions{exportHead: &head})
			gh := newFakeGitHub(t)
			p := storeBackedPublisher(t, st, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)})
			candidate := testCandidate(t)
			candidate.RunID = reservation.RunID
			candidate.DispositionHistory = testDispositionHistory(t, st, candidate)
			stopped := false
			stop := func() {
				if !stopped {
					stopped = true
					stopPublicationTask(t, st, candidate.RunID)
				}
			}
			if boundary == "before-intent" {
				stop()
			}
			gh.onRequest = func(method, path string) {
				if path == testRepoPath+"/pulls" && ((boundary == "before-pr" && method == http.MethodGet) || (boundary == "in-flight-pr" && method == http.MethodPost)) {
					stop()
				}
			}
			result, err := p.PublishExecutionAfterGateAndFinalize(t.Context(), publish.ExecutionCandidate{Candidate: candidate, ProducingInvocationID: testProducingInvocationID}, testApprovedRecipes(), func(context.Context, publish.GatedHead) error {
				if boundary == "after-intent" {
					stop()
				}
				return nil
			})
			if !stopped {
				t.Fatal("race fixture did not reach Stop boundary")
			}
			if boundary == "in-flight-pr" {
				if err != nil || result.PRNumber == 0 {
					t.Fatalf("lost in-flight publication evidence: %+v, %v", result, err)
				}
				writes := len(gh.writeRequests())
				if err := p.ReconcileCancelledRun(t.Context(), candidate.RunID); err != nil {
					t.Fatal(err)
				}
				if len(gh.writeRequests()) != writes {
					t.Fatal("cancellation recovery mutated forge")
				}
				return
			}
			if !errors.Is(err, store.ErrTaskCancellationFenced) {
				t.Fatalf("publication crossed fence: %v", err)
			}
			for _, write := range gh.writeRequests() {
				if write == http.MethodPost+" "+testRepoPath+"/pulls" {
					t.Fatal("created PR after Stop")
				}
			}
			if boundary == "before-intent" || boundary == "after-intent" {
				if len(gh.writeRequests()) != 0 {
					t.Fatalf("post-fence writes: %v", gh.writeRequests())
				}
			}
			if boundary != "before-intent" {
				before := len(gh.writeRequests())
				if err := p.ReconcileCancelledRun(t.Context(), candidate.RunID); err == nil {
					t.Fatal("missing external outcome claimed settled")
				}
				if len(gh.writeRequests()) != before {
					t.Fatal("uncertain recovery retried a forbidden write")
				}
			}
		})
	}
}

// TestTaskCancellationFencesDraftMutationAfterContentPatch pins the per-write
// cancellation fence for the draft repair under a MANAGED draft intent (the
// Part D case; Part C production keeps draft unmanaged, so this drives
// convergePR directly through the test seam). When content and draft state both
// drift, convergePR patches the body and then toggles draft; the content PATCH
// is a forge round-trip during which the task can be stopped. The draft
// mutation must re-check the durable fence and fail closed, never releasing the
// hold on a cancelled task's PR after the batch's first fence check.
func TestTaskCancellationFencesDraftMutationAfterContentPatch(t *testing.T) {
	managed := func(b bool) *bool { return &b }
	head := testHeadSHA
	st := newExecutionBoundStore(t, executionChainOptions{})
	seedDecisionRecords(t, st)
	reservation := seedExecutionPublicationChain(t, st, executionChainOptions{exportHead: &head})
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, st, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)})
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, st, candidate)
	identity := testCandidateIdentity(t)

	// A stale, still-draft owned PR: convergePR patches content, then a managed
	// not-draft intent would mark it ready. The marker keeps it recognized as
	// ours before and after the patch.
	gh.prs = append(gh.prs, fakePR{
		Number:  4242,
		State:   "open",
		Title:   "Stale title",
		Body:    identity.Marker(),
		HeadRef: identity.BranchName(),
		HeadSHA: candidate.HeadSHA,
		Draft:   true,
		NodeID:  "PR_node_4242",
	})
	title := "Fresh title"
	body := "Fresh prose.\n\n" + identity.Marker()

	stopped := false
	draftMutationAfterStop := false
	gh.onRequest = func(method, path string) {
		if stopped && method == http.MethodPost && path == "/graphql" {
			draftMutationAfterStop = true
			return
		}
		if !stopped && method == http.MethodPatch && strings.HasPrefix(path, testRepoPath+"/pulls/") {
			stopped = true
			stopPublicationTask(t, st, candidate.RunID)
		}
	}

	_, _, err := p.ConvergePRForTest(
		t.Context(), candidate.Repo, identity, candidate, title, body, managed(false), false, 4242,
	)

	if !stopped {
		t.Fatal("fixture never reached the in-flight content PATCH")
	}
	if !errors.Is(err, store.ErrTaskCancellationFenced) {
		t.Fatalf("draft repair crossed the fence: %v", err)
	}
	if draftMutationAfterStop {
		t.Fatal("setPRDraft ran after Stop: draft mutation escaped the cancellation fence")
	}
}

func TestTaskCancellationObservesUnrecordedPublishedPR(t *testing.T) {
	head := testHeadSHA
	st := newExecutionBoundStore(t, executionChainOptions{})
	seedDecisionRecords(t, st)
	reservation := seedExecutionPublicationChain(t, st, executionChainOptions{exportHead: &head})
	gh := newFakeGitHub(t)
	p := storeBackedPublisher(t, st, gh, fixedWorkflowAuditor{audit: executionWorkflowAudit(t)})
	candidate := testCandidate(t)
	candidate.RunID = reservation.RunID
	candidate.DispositionHistory = testDispositionHistory(t, st, candidate)
	// Leave the successful external effect's local outbox entry pending, as
	// when the daemon exits before recording the returned PR identity.
	result, err := p.PublishExecution(t.Context(), publish.ExecutionCandidate{Candidate: candidate, ProducingInvocationID: testProducingInvocationID}, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingPublications(t, st)) != 1 {
		t.Fatal("fixture did not retain an unresolved publication intent")
	}
	stopPublicationTask(t, st, candidate.RunID)
	writes := len(gh.writeRequests())
	if err := p.ReconcileCancelledRun(t.Context(), candidate.RunID); err != nil {
		t.Fatal(err)
	}
	if len(gh.writeRequests()) != writes || len(pendingPublications(t, st)) != 0 {
		t.Fatal("recovery repeated a forge write or failed to retain the outcome")
	}
	assertOutcomeRecorded(t, st, publish.OutcomeKey(result.Identity), publish.Outcome{
		Identity: result.Identity.Digest(), Repo: candidate.Repo, BaseRef: candidate.BaseRef,
		HeadSHA: candidate.HeadSHA, Branch: result.Branch, PRNumber: result.PRNumber,
		EvidenceEligible: true,
	})
}
