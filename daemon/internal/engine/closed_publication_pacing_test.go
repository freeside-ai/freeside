package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// closedPublicationSchedule is the attempt schedule at the default
// holdRetryInterval over the 10 minutes each phase drives: an immediate
// attempt, then 30 s doubling. Unpaced, the same 100 ms passes made about
// 6,000 attempts (#2343).
var closedPublicationSchedule = []time.Duration{
	0, 30 * time.Second, 90 * time.Second, 210 * time.Second, 450 * time.Second,
}

const closedPublicationWarning = "closed workflow publication observation remains unresolved"

type closedPublicationHarness struct {
	t      *testing.T
	store  *store.Store
	runID  domain.RunID
	logs   *bytes.Buffer
	result error
	// attemptAt records each reconcile attempt's offset from the phase start.
	attemptAt []time.Duration
	start     time.Time
}

func newClosedPublicationHarness(t *testing.T) *closedPublicationHarness {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-closed-publication")
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutRun(t.Context(), domain.Run{
			ID: runID, ProjectID: "project-closed-publication",
			SpecDigest:   domain.Digest(contentaddr.Sum([]byte("specification"))),
			PolicyDigest: domain.Digest(contentaddr.Sum([]byte("policy"))),
			Stages: []domain.Stage{{
				ID: "stage-implementation", RunID: runID, Name: "implementation",
			}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	closeWorkflowForTest(t, st, runID, false)
	return &closedPublicationHarness{t: t, store: st, runID: runID, logs: &bytes.Buffer{}}
}

// workflow builds a lane over the shared store with fresh process state, as
// a new deputy start on the same root would have.
func (h *closedPublicationHarness) workflow() *productionPublicationWorkflow {
	return &productionPublicationWorkflow{
		store: h.store, logger: slog.New(slog.NewTextHandler(h.logs, nil)),
		holdRetryInterval:      defaultProductionHoldRetryInterval,
		now:                    time.Now,
		closedPublicationRetry: make(map[domain.RunID]closedPublicationPace),
		reconcileClosedPublication: func(_ context.Context, runID domain.RunID) error {
			if runID != h.runID {
				h.t.Fatalf("observed run %q, want %q", runID, h.runID)
			}
			h.attemptAt = append(h.attemptAt, time.Since(h.start))
			return h.result
		},
	}
}

// drive runs the sweep every 100 ms for d, the lane's cadence, after
// resetting the phase's attempt record. onAttempt runs after each pass that
// made an attempt, with the attempt's 1-based number.
func (h *closedPublicationHarness) drive(
	w *productionPublicationWorkflow, d time.Duration, onAttempt func(attempt int),
) {
	h.t.Helper()
	h.attemptAt, h.start = nil, time.Now()
	for elapsed := time.Duration(0); elapsed < d; elapsed += 100 * time.Millisecond {
		before := len(h.attemptAt)
		if err := w.observeClosedWorkflows(h.t.Context()); err != nil {
			h.t.Fatal(err)
		}
		if onAttempt != nil && len(h.attemptAt) > before {
			onAttempt(len(h.attemptAt))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *closedPublicationHarness) hold() (domain.RunHoldObservation, bool) {
	h.t.Helper()
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := h.store.Read(h.t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(h.t.Context(), h.runID)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return hold, found
}

func (h *closedPublicationHarness) requireSchedule(phase string) {
	h.t.Helper()
	if fmt.Sprint(h.attemptAt) != fmt.Sprint(closedPublicationSchedule) {
		h.t.Fatalf("%s: attempts at %v, want %v", phase, h.attemptAt, closedPublicationSchedule)
	}
}

// TestClosedPublicationObservationIsPacedAndHeld pins #2343's bound: a closed
// run whose publication cannot settle is observed on the backoff schedule,
// gains a durable external_conflict hold on its third unsettled attempt, keeps
// that hold and the schedule across a restart, and loses the hold once the
// intent settles.
func TestClosedPublicationObservationIsPacedAndHeld(t *testing.T) {
	if defaultProductionHoldRetryInterval != 30*time.Second {
		t.Fatalf("default hold-retry interval = %v; closedPublicationSchedule assumes 30s",
			defaultProductionHoldRetryInterval)
	}
	synctest.Test(t, func(t *testing.T) {
		h := newClosedPublicationHarness(t)
		h.result = fmt.Errorf("publication publish-run has no exact settled external outcome: %w",
			publish.ErrPublicationUnsettled)

		var first, last time.Time
		h.drive(h.workflow(), 10*time.Minute, func(attempt int) {
			hold, found := h.hold()
			switch {
			case attempt < closedPublicationUnsettledTolerance:
				if found {
					t.Fatalf("attempt %d: hold %+v recorded before the tolerance", attempt, hold)
				}
				return
			case !found || hold.Reason != domain.HoldExternalConflict || hold.InvocationID != nil:
				t.Fatalf("attempt %d: hold = %+v, found %v, want external_conflict", attempt, hold, found)
			case attempt == closedPublicationUnsettledTolerance:
				first = hold.FirstObservedAt
			case !hold.FirstObservedAt.Equal(first) || !hold.LastObservedAt.After(last):
				t.Fatalf("attempt %d: hold span %v..%v, want first %v and last after %v",
					attempt, hold.FirstObservedAt, hold.LastObservedAt, first, last)
			}
			last = hold.LastObservedAt
		})
		h.requireSchedule("first start")
		if got := strings.Count(h.logs.String(), closedPublicationWarning); got != len(closedPublicationSchedule) {
			t.Fatalf("logged %d warnings, want one per attempt (%d)", got, len(closedPublicationSchedule))
		}

		// A new start keeps the durable hold and begins its own schedule
		// with one immediate attempt.
		restarted := h.workflow()
		if hold, found := h.hold(); !found || !hold.FirstObservedAt.Equal(first) {
			t.Fatalf("hold after restart = %+v, found %v", hold, found)
		}
		h.drive(restarted, 10*time.Minute, nil)
		h.requireSchedule("restart")
		if hold, found := h.hold(); !found || !hold.FirstObservedAt.Equal(first) || !hold.LastObservedAt.After(last) {
			t.Fatalf("hold after restarted attempts = %+v, found %v", hold, found)
		}

		// The next due attempt, 7 m 30 s after the last one, settles.
		h.result = nil
		h.drive(restarted, 10*time.Minute, nil)
		if len(h.attemptAt) == 0 {
			t.Fatal("no attempt after the backoff elapsed")
		}
		if hold, found := h.hold(); found {
			t.Fatalf("hold %+v survived its intent settling", hold)
		}
		if len(restarted.closedPublicationRetry) != 0 {
			t.Fatalf("settled run kept pacing state: %+v", restarted.closedPublicationRetry)
		}
	})
}

// TestClosedPublicationObservationPacesOtherErrors pins that a failure other
// than an unsettled outcome, such as GitHub being unreachable, backs off on
// the same schedule and is logged per attempt but asks the operator nothing.
func TestClosedPublicationObservationPacesOtherErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newClosedPublicationHarness(t)
		h.result = errors.New("list pulls: mint: installation resolution: GET /app/installations")
		h.drive(h.workflow(), 10*time.Minute, nil)
		h.requireSchedule("transient failure")
		if got := strings.Count(h.logs.String(), closedPublicationWarning); got != len(closedPublicationSchedule) {
			t.Fatalf("logged %d warnings, want one per attempt (%d)", got, len(closedPublicationSchedule))
		}
		if hold, found := h.hold(); found {
			t.Fatalf("transient failure recorded hold %+v", hold)
		}
	})
}

// TestClosedPublicationObservationDropsPacingOnSettle pins that a run whose
// observation settles after only non-sentinel failures leaves no pacing
// state behind, so a settled run is never skipped by a stale due time.
func TestClosedPublicationObservationDropsPacingOnSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newClosedPublicationHarness(t)
		w := h.workflow()
		h.result = errors.New("list pulls: connection reset")
		h.drive(w, time.Minute, nil)
		if len(w.closedPublicationRetry) != 1 {
			t.Fatalf("failing run pacing state = %+v, want one entry", w.closedPublicationRetry)
		}
		h.result = nil
		h.drive(w, time.Minute, nil)
		if len(h.attemptAt) == 0 {
			t.Fatal("no attempt after the backoff elapsed")
		}
		if len(w.closedPublicationRetry) != 0 {
			t.Fatalf("settled run kept pacing state: %+v", w.closedPublicationRetry)
		}
	})
}

// TestClosedPublicationObservationKeepsBlockedTaskHold pins the #2343
// incident's shape: the task was held at publication with external_conflict
// before it was abandoned. The sweep's hold keeps that row's invocation and
// first instant, so the operator still sees when the conflict began.
func TestClosedPublicationObservationKeepsBlockedTaskHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newClosedPublicationHarness(t)
		blockedAt := time.Now().UTC().Add(-time.Hour)
		publication := domain.InvocationID("publish-run-closed-publication")
		if err := h.store.Write(t.Context(), func(tx *store.WriteTx) error {
			return recordRunHold(t.Context(), tx, h.runID, publication,
				domain.HoldExternalConflict, holdDetail{}, blockedAt)
		}); err != nil {
			t.Fatal(err)
		}
		h.result = fmt.Errorf("publication %s has no exact settled external outcome: %w",
			publication, publish.ErrPublicationUnsettled)
		h.drive(h.workflow(), 10*time.Minute, nil)
		h.requireSchedule("blocked task")
		hold, found := h.hold()
		if !found || hold.Reason != domain.HoldExternalConflict ||
			hold.InvocationID == nil || *hold.InvocationID != publication ||
			!hold.FirstObservedAt.Equal(blockedAt) || !hold.LastObservedAt.After(blockedAt) {
			t.Fatalf("hold = %+v, found %v; want the blocked task's hold, first at %v, observed since",
				hold, found, blockedAt)
		}
	})
}
