package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedTerminalRun records a run with a real terminal authority (admission +
// export + terminal_recorded milestone) and one invocation observation. The
// observed status may lag the terminal authority; that is legitimate paced
// observation staleness, and the served projection derives the terminal status.
func (f corpusFixture) seedTerminalRun(
	t *testing.T, runID domain.RunID, invocation domain.InvocationID,
	terminal, observed domain.ObservedInvocationStatus,
) {
	t.Helper()
	ctx := context.Background()
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, invocation)))
	})
	admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
	f.seedExport(t, admission)
	f.appendMilestone(t, domain.RunMilestone{
		RunID: runID, Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
		Terminal: ptr(terminal), RecordedAt: f.at.Add(2 * time.Hour),
	})
	live := observed == domain.ObservedStatusRunning
	f.observe(t, domain.InvocationObservation{
		InvocationID: invocation, RunID: runID, Status: observed,
		Live: live, ObservedAt: f.at.Add(2 * time.Hour),
	})
}

// seedObservationBindingFailureRun records a structurally valid observation
// for an invocation the run does not own. Unlike a status lag, this is a genuine
// returned-object binding failure and must remain excluded by #767.
func (f corpusFixture) seedObservationBindingFailureRun(t *testing.T, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	owned := domain.InvocationID("inv-owned-" + string(runID))
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, corpusRun(runID, corpusAttempt(runID, owned)))
	})
	f.observe(t, domain.InvocationObservation{
		InvocationID: domain.InvocationID("inv-stranger-" + string(runID)), RunID: runID,
		Status: domain.ObservedStatusRunning, Live: false, ObservedAt: f.at,
	})
}

// TestRunObservationIntegrityRaisesTypedSentinel pins that a run whose
// observation row is not bound to one of its attempts fails the authenticated
// projection with ErrRunObservationIntegrity, chained over the underlying
// ErrParentKeyMismatch so both remain matchable (#767). The single-run reads
// return this differentiated failure rather than a false-empty.
func TestRunObservationIntegrityRaisesTypedSentinel(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	runID := domain.RunID("run-contradiction")
	f.seedObservationBindingFailureRun(t, runID)

	for _, tc := range []struct {
		name string
		read func() error
	}{
		{"GetRunTimeline", func() error { _, err := f.service.GetRunTimeline(ctx, runID); return err }},
		{"GetRun", func() error { _, err := f.service.GetRun(ctx, runID); return err }},
	} {
		err := tc.read()
		if !errors.Is(err, signet.ErrRunObservationIntegrity) {
			t.Fatalf("%s error = %v, want ErrRunObservationIntegrity", tc.name, err)
		}
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("%s error = %v, want underlying ErrParentKeyMismatch preserved", tc.name, err)
		}
	}
}

// TestListingReadsIsolateDamagedRun is #767's core acceptance: one run whose
// observation row names an invocation it does not own is excluded from Bootstrap and
// ListRuns instead of failing them, the healthy sibling is still served, and
// the contradiction is logged durably naming the excluded run.
func TestListingReadsIsolateDamagedRun(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := newCorpusFixture(t, signet.WithLogger(logger))
	healthy := domain.RunID("run-healthy")
	damaged := domain.RunID("run-damaged")
	f.seedAuthIdentity(t)
	f.seedTerminalRun(t, healthy, "inv-healthy", domain.ObservedStatusCompleted, domain.ObservedStatusCompleted)
	f.seedObservationBindingFailureRun(t, damaged)

	assertOnlyHealthy := func(t *testing.T, ids []domain.RunID) {
		t.Helper()
		if len(ids) != 1 || ids[0] != healthy {
			t.Fatalf("served runs = %v, want only %q", ids, healthy)
		}
	}

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap over a damaged run = %v, want the healthy run served", err)
	}
	bootstrapIDs := make([]domain.RunID, 0, len(bootstrap.Runs))
	for _, run := range bootstrap.Runs {
		bootstrapIDs = append(bootstrapIDs, run.Run.ID)
	}
	assertOnlyHealthy(t, bootstrapIDs)

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns over a damaged run = %v, want the healthy run served", err)
	}
	listIDs := make([]domain.RunID, 0, len(runs))
	for _, run := range runs {
		listIDs = append(listIDs, run.Run.ID)
	}
	assertOnlyHealthy(t, listIDs)

	out := logs.String()
	if !strings.Contains(out, "excluding run from listing") || !strings.Contains(out, string(damaged)) {
		t.Fatalf("logs = %q, want a warn naming excluded run %q", out, damaged)
	}
	if strings.Contains(out, string(healthy)) {
		t.Fatalf("logs = %q, healthy run %q must not be logged as excluded", out, healthy)
	}
}

// TestHTTPBootstrapExcludesDamagedRunReturns200 pins the operator-facing
// symptom fixed by #767: an authenticated /sync/bootstrap against a store with
// a damaged legacy run returns 200 with the healthy runs, not the
// undifferentiated 500 that surfaced as a "daemon unreachable" banner.
func TestHTTPBootstrapExcludesDamagedRunReturns200(t *testing.T) {
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	f.seedTerminalRun(t, "run-healthy", "inv-healthy", domain.ObservedStatusCompleted, domain.ObservedStatusCompleted)
	f.seedObservationBindingFailureRun(t, "run-damaged")

	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := authenticatedRequest(t, handler, http.MethodGet, "/sync/bootstrap", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var bootstrap signet.BootstrapSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if len(bootstrap.Runs) != 1 || bootstrap.Runs[0].Run.ID != "run-healthy" {
		t.Fatalf("bootstrap runs = %+v, want only run-healthy", bootstrap.Runs)
	}
}
