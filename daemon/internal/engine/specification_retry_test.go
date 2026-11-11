package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const retryReason = "The provider recovered from its outage."

// failSpecificationIteration makes the driver fail the invocation, then
// reconciles once so the engine records the failure and its card.
func failSpecificationIteration(t *testing.T, f specificationFixture, driver *execfake.StageDriver, e *Engine, iteration int) {
	t.Helper()
	driver.Script(specificationInvocationID("specification-run", iteration), execfake.StageScript{
		Outcome: execfake.OutcomeFail,
		Result:  exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Specifier fetch timed out."},
	})
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSpecificationFailedWithoutImplementation(t, f, specificationInvocationID("specification-run", iteration))
}

func scriptSpecificationOutput(t *testing.T, driver *execfake.StageDriver, iteration int, body string) {
	t.Helper()
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", iteration), 0, 0, specify.Output{
		Specification: &specify.Specification{Summary: "Ready.", Body: body, Addressals: []specify.Addressal{}},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f specificationFixture) retry(t *testing.T, reason string) (SpecificationRetry, error) {
	t.Helper()
	return RetrySpecificationRun(t.Context(), f.store, SpecificationRetrySpec{
		SpecificationRunID: "specification-run", Reason: reason,
	})
}

func (f specificationFixture) revision(t *testing.T) int64 {
	t.Helper()
	state, err := f.store.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

func (f specificationFixture) request(t *testing.T, iteration int) specificationRequest {
	t.Helper()
	var request specificationRequest
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(specificationInvocationID("specification-run", iteration)))
		if err != nil {
			return err
		}
		request, err = decodeSpecificationRequest(entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return request
}

// requireRetryRefused checks that the retry fails with want and writes
// nothing.
func requireRetryRefused(t *testing.T, f specificationFixture, reason string, want error) {
	t.Helper()
	before := f.revision(t)
	if _, err := f.retry(t, reason); !errors.Is(err, want) {
		t.Fatalf("retry error = %v, want %v", err, want)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("refused retry moved the store revision from %d to %d", before, after)
	}
}

func TestSpecificationRetryDispatchesToApproval(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 3)
	driver := f.newDriver(t)
	e := f.newEngine(t, driver)
	f.submit(t)
	failSpecificationIteration(t, f, driver, e, 1)
	failed := specificationInvocationID("specification-run", 1)

	retried, err := f.retry(t, retryReason)
	if err != nil {
		t.Fatal(err)
	}
	second := specificationInvocationID("specification-run", 2)
	if !retried.Created || retried.InvocationID != second || retried.RetriedInvocationID != failed ||
		retried.Run.ID != "specification-run" || retried.StageID != specificationStageID("specification-run") ||
		retried.SourceArtifactID != f.source.ID || retried.PolicyArtifactID != f.policyArt.ID {
		t.Fatalf("retry = %+v", retried)
	}
	first, next := f.request(t, 1), f.request(t, 2)
	if next.RetryReason != retryReason || !slices.Equal(next.InputArtifactIDs, first.InputArtifactIDs) {
		t.Fatalf("retry request = %+v, want the failed inputs and the reason", next)
	}

	card, _ := f.item(t, specificationFailureItemID(failed))
	if card.Status != domain.StatusSuperseded || card.Conclusion == nil ||
		card.Conclusion.Cause != domain.ConcludedBySupersession ||
		!strings.Contains(card.Reason, "specification iteration 2") || !strings.Contains(card.Reason, retryReason) ||
		strings.Contains(card.Conclusion.Reason, retryReason) {
		t.Fatalf("failure card = %+v", card)
	}

	// Repeating the command with the same reason finds the pending retry.
	before := f.revision(t)
	replayed, err := f.retry(t, retryReason)
	if err != nil || replayed.Created || replayed.InvocationID != second || replayed.RetriedInvocationID != failed {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("replay moved the store revision from %d to %d", before, after)
	}
	requireRetryRefused(t, f, "Another reason.", ErrSpecificationNotRetryable)

	scriptSpecificationOutput(t, driver, 2, "# Specification\n\nImplement after the retry.")
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	approval, _ := f.item(t, "spec-approval-implementation-run-2")
	if approval.Status != domain.StatusOpen {
		t.Fatalf("approval after retry = %+v", approval)
	}
	requireAttemptPurposes(t, f.store, "specification-run", domain.AttemptPurposeFirstDraft, domain.AttemptPurposeRetry)
	// A replayed dispatch of a recorded invocation leaves its attempt, purpose
	// included, as it was.
	if _, _, _, err := e.recordAttempt(t.Context(), "specification-run", specificationStageID("specification-run"),
		second, new(domain.AttemptPurposeDiscussionReply), "dispatched", nil); err != nil {
		t.Fatalf("replayed recordAttempt: %v", err)
	}
	requireAttemptPurposes(t, f.store, "specification-run", domain.AttemptPurposeFirstDraft, domain.AttemptPurposeRetry)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		facts, err := SpecificationRetries(t.Context(), tx, "specification-run")
		if err != nil {
			return err
		}
		want := []SpecificationRetryFact{{InvocationID: second, RetriedInvocationID: failed, Reason: retryReason}}
		if !slices.Equal(facts, want) {
			t.Errorf("retries = %+v, want %+v", facts, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A completed invocation awaiting approval is not retried.
	requireRetryRefused(t, f, retryReason, ErrSpecificationNotRetryable)
}

func TestSpecificationRetryAcceptsCanceledAndLostTerminals(t *testing.T) {
	t.Parallel()
	for _, status := range []exec.Status{exec.StatusCanceled, exec.StatusGone} {
		t.Run(string(status), func(t *testing.T) {
			f := newSpecificationFixture(t, true, 2)
			f.submit(t)
			e := f.newEngine(t, f.newDriver(t))
			run, err := f.run("specification-run")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.recordSpecificationFailure(t.Context(), run, f.request(t, 1), status, "ended"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.retry(t, retryReason); err != nil {
				t.Fatal(err)
			}
			entry := store.QueueEntry{}
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				entry, err = tx.GetOutbox(t.Context(), string(specificationInvocationID("specification-run", 2)))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.loadSpecificationBinding(t.Context(), entry); err != nil {
				t.Fatalf("retry binding = %v", err)
			}
		})
	}
}

func TestSpecificationRetryRefusals(t *testing.T) {
	t.Parallel()
	recordTerminal := func(t *testing.T, f specificationFixture, status exec.Status) {
		t.Helper()
		run, err := f.run("specification-run")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.newEngine(t, f.newDriver(t)).recordSpecificationFailure(
			t.Context(), run, f.request(t, 1), status, "ended"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("pending", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		requireRetryRefused(t, f, retryReason, ErrSpecificationNotRetryable)
	})
	t.Run("blocked", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		recordTerminal(t, f, exec.StatusBlocked)
		requireRetryRefused(t, f, retryReason, ErrSpecificationNotRetryable)
	})
	t.Run("budget spent", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 1)
		f.submit(t)
		recordTerminal(t, f, exec.StatusFailed)
		requireRetryRefused(t, f, retryReason, ErrSpecificationIterationsExhausted)
	})
	t.Run("card already concluded", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		recordTerminal(t, f, exec.StatusFailed)
		card, _ := f.item(t, specificationFailureItemID(specificationInvocationID("specification-run", 1)))
		card.ItemVersion++
		card, err := card.ConcludeByDaemon(domain.ConcludedByDaemon, time.Now().UTC(), "Concluded elsewhere.", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(t.Context(), card)
		}); err != nil {
			t.Fatal(err)
		}
		requireRetryRefused(t, f, retryReason, ErrSpecificationNotRetryable)
	})
	t.Run("cancelled task", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		recordTerminal(t, f, exec.StatusFailed)
		stopSpecificationTask(t, f)
		requireRetryRefused(t, f, retryReason, store.ErrTaskCancellationFenced)
	})
	t.Run("closed workflow", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		recordTerminal(t, f, exec.StatusFailed)
		closeWorkflowForTest(t, f.store, "specification-run", false)
		requireRetryRefused(t, f, retryReason, store.ErrTaskWorkflowClosed)
	})
	t.Run("unspecified run", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		if _, err := f.retry(t, retryReason); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("retry of a missing run = %v", err)
		}
		if _, err := f.retry(t, " padded "); err == nil {
			t.Fatal("retry accepted an untrimmed reason")
		}
	})
}

func stopSpecificationTask(t *testing.T, f specificationFixture) {
	t.Helper()
	ctx := t.Context()
	run, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "stop-retry", DeviceID: "device-1", TaskID: run.TaskID, ProjectID: run.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSpecificationRetryKeepsLaterTransitions checks that the reason stays on
// its own iteration: request_changes after a retried specification builds an
// iteration the chain check accepts.
func TestSpecificationRetryKeepsLaterTransitions(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 3)
	driver := f.newDriver(t)
	e := f.newEngine(t, driver)
	f.submit(t)
	failSpecificationIteration(t, f, driver, e, 1)
	if _, err := f.retry(t, retryReason); err != nil {
		t.Fatal(err)
	}
	scriptSpecificationOutput(t, driver, 2, "# Specification\n\nFirst draft.")
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	item, snapshot := f.item(t, "spec-approval-implementation-run-2")
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-retried-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionRequestChanges,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests, Message: "Name the tests.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 3), 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary: "Revised.", Body: "# Specification\n\nSecond draft, with tests.",
			Addressals: []specify.Addressal{{CommentID: "revise-retried-spec", Response: "Named the tests."}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := e.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if third := f.request(t, 3); third.RetryReason != "" || len(third.FeedbackArtifactIDs) != 1 {
		t.Fatalf("revision after retry = %+v", third)
	}
	approval, _ := f.item(t, "spec-approval-implementation-run-3")
	if approval.Status != domain.StatusOpen {
		t.Fatalf("approval after revision = %+v", approval)
	}
}

// TestLoadSpecificationBindingRejectsForgedRetry checks the chain refuses a
// retry row the retry write could not have produced.
func TestLoadSpecificationBindingRejectsForgedRetry(t *testing.T) {
	t.Parallel()
	failed := specificationInvocationID("specification-run", 1)
	for _, tc := range []struct {
		name      string
		edit      func(next *specificationRequest, research domain.ArtifactID)
		terminal  []domain.ArtifactID
		supersede bool
		want      string
	}{
		{
			name: "changed inputs", supersede: true, want: "is not a retry of",
			edit: func(next *specificationRequest, research domain.ArtifactID) {
				next.InputArtifactIDs = append(next.InputArtifactIDs, research)
			},
		},
		{
			name: "no reason", supersede: true, want: "is not a retry of",
			edit: func(next *specificationRequest, _ domain.ArtifactID) { next.RetryReason = "" },
		},
		{
			name: "card still open", want: "has no superseded failure card",
			edit: func(*specificationRequest, domain.ArtifactID) {},
		},
		{
			name: "reason on a research transition", terminal: []domain.ArtifactID{"research-retry-1"},
			want: "not authorized by its preceding transition",
			edit: func(next *specificationRequest, research domain.ArtifactID) {
				next.InputArtifactIDs = append(next.InputArtifactIDs, research)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpecificationFixture(t, true, 3)
			f.submit(t)
			e := f.newEngine(t, f.newDriver(t))
			research := testSpecificationArtifact(t, "research-retry-1", domain.ArtifactKindResearch,
				domain.Digest(contentaddr.Sum([]byte("research"))), domain.ProducerDaemon, failed)
			first := f.request(t, 1)
			if tc.terminal == nil {
				run, err := f.run("specification-run")
				if err != nil {
					t.Fatal(err)
				}
				if err := e.recordSpecificationFailure(t.Context(), run, first, exec.StatusFailed, "failed"); err != nil {
					t.Fatal(err)
				}
			}
			next := nextSpecificationRetryRequest(first, retryReason)
			tc.edit(&next, research.ID)
			payload, err := encodeSpecificationRequest(next)
			if err != nil {
				t.Fatal(err)
			}
			invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				if err := tx.PutArtifact(t.Context(), research); err != nil {
					return err
				}
				if tc.terminal != nil {
					body, err := encodeSpecificationTerminal(specificationTerminal{
						InvocationID: failed, Iteration: 1, Status: exec.StatusCompleted,
						ResearchArtifactIDs: tc.terminal,
					})
					if err != nil {
						return err
					}
					if _, _, err := tx.RecordInbox(t.Context(), string(failed), kindSpecificationTerminal, body); err != nil {
						return err
					}
					if err := tx.MarkOutboxDispatched(t.Context(), string(failed)); err != nil {
						return err
					}
				}
				if tc.supersede {
					card, err := tx.GetAttentionItem(t.Context(), specificationFailureItemID(failed))
					if err != nil {
						return err
					}
					card.ItemVersion++
					if card, err = card.ConcludeByDaemon(
						domain.ConcludedBySupersession, time.Now().UTC(), "Superseded.", nil); err != nil {
						return err
					}
					if err := tx.PutAttentionItem(t.Context(), card); err != nil {
						return err
					}
				}
				if err := tx.PutAgentInvocation(t.Context(), invocation); err != nil {
					return err
				}
				_, _, err := tx.EnqueueOutbox(t.Context(), string(next.InvocationID), KindSpecificationInvocationRequested, payload)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var entry store.QueueEntry
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				entry, err = tx.GetOutbox(t.Context(), string(next.InvocationID))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.loadSpecificationBinding(t.Context(), entry); !errors.Is(err, domain.ErrParentKeyMismatch) ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("forged retry binding = %v, want ErrParentKeyMismatch naming %q", err, tc.want)
			}
		})
	}
}

func TestSpecificationRequestRefusesRetryReasonOnFirstIteration(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 2)
	f.submit(t)
	first := f.request(t, 1)
	first.RetryReason = retryReason
	if _, err := encodeSpecificationRequest(first); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("first-iteration retry reason = %v, want ErrParentKeyMismatch", err)
	}
	second := nextSpecificationRetryRequest(f.request(t, 1), " padded ")
	if _, err := encodeSpecificationRequest(second); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("untrimmed retry reason = %v, want ErrParentKeyMismatch", err)
	}
}
