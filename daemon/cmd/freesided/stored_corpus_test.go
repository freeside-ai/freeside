package main

import (
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The stored-state corpus holds databases that real earlier builds of the
// scenario daemon wrote, each stopped with work in flight. Every build must
// start on each one, read its history as a client would, and carry its work
// to a published pull request (daemon/README.md, Stored-State Corpus). A
// fixture set lives in <corpus>/<YYYYMMDD>-<sha12>/<case>/: the database as
// freeside.db.gz, each of storedCorpusTrees the daemon wrote, and
// fixture.json.
const (
	storedCorpusDatabase = "freeside.db.gz"
	storedCorpusManifest = "fixture.json"
)

// storedCorpusTrees are the directories beside the database a fixture
// carries, relative to the daemon's root: the blob store, and the durable
// state of the scenario's fake stage driver and review source. The fakes
// stand in for the driver's and reviewer's own records, which a resume
// needs: every pass re-authenticates a recorded attempt of an open run
// against the driver, and a recorded review against the reviewer.
var storedCorpusTrees = []string{"freeside.db.blobs", "scenario/stage-driver", "scenario/review-source"}

// storedCorpusFixture is fixture.json: which build wrote the fixture, at
// which schema, how, and the task and run its resume continues.
type storedCorpusFixture struct {
	BuildCommit   string        `json:"build_commit"`
	SchemaVersion int           `json:"schema_version"`
	Case          string        `json:"case"`
	ProducedAt    time.Time     `json:"produced_at"`
	ProducedBy    string        `json:"produced_by"`
	TaskID        domain.TaskID `json:"task_id"`
	RunID         domain.RunID  `json:"run_id"`
}

// storedCorpusCase is one in-flight state the corpus captures: the scenario
// that plays it and how the current build carries it to a published pull
// request. The producer's stop point for each case is in
// storedCorpusProducers; a new case adds both in the same change.
type storedCorpusCase struct {
	scenario func() daemoncompose.Scenario
	resume   func(t *testing.T, h *daemon, fixture storedCorpusFixture)
}

var storedCorpusCases = map[string]storedCorpusCase{
	// A run stopped between its specification and implementation stages,
	// waiting on an open specification-approval item.
	"spec-approval": {
		scenario: daemoncompose.CleanScenario,
		resume: func(t *testing.T, h *daemon, fixture storedCorpusFixture) {
			t.Helper()
			approveScenarioSpecification(t, h, fixture.RunID)
			assertScenarioPublished(t, h.Attention, fixture.TaskID, fixture.RunID)
		},
	},
	// An implementation attempt parked on a provider quota window.
	"quota-park": {
		scenario: daemoncompose.QuotaParkScenario,
		resume: func(t *testing.T, h *daemon, fixture storedCorpusFixture) {
			t.Helper()
			// The fixture's window reset passed long ago, so the park may
			// already have released. A hold still in place is released the
			// way TestScenarioQuotaParkResumes does, by an allowed reading.
			var (
				hold  domain.RunHoldObservation
				found bool
			)
			if err := h.Store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				hold, found, err = tx.GetRunHold(t.Context(), fixture.RunID)
				return err
			}); err != nil {
				t.Fatalf("read hold on %s: %v", fixture.RunID, err)
			}
			if found && hold.QuotaWindow != nil {
				recordScenarioAllowedReading(t, h.Store, hold.QuotaWindow.Pool, hold.QuotaWindow.Window)
			}
			assertScenarioPublished(t, h.Attention, fixture.TaskID, fixture.RunID)
		},
	},
	// A production publication task recorded before any forge effect.
	"publication-pending": {
		scenario: daemoncompose.CleanScenario,
		resume: func(t *testing.T, h *daemon, fixture storedCorpusFixture) {
			t.Helper()
			assertScenarioPublished(t, h.Attention, fixture.TaskID, fixture.RunID)
		},
	},
}

// assertNoDurableStop fails when the daemon recorded a durable stop: a
// component that met state it couldn't handle stopped, even when the rest
// of the daemon carried on.
func assertNoDurableStop(t *testing.T, h *daemon) {
	t.Helper()
	items, err := h.Attention.ListAttentionItems(t.Context())
	if err != nil {
		t.Fatalf("list attention items: %v", err)
	}
	for _, item := range items {
		if strings.HasPrefix(string(item.Item.ID), daemonshell.DurableStopItemPrefix) {
			t.Fatalf("daemon recorded a durable stop: %+v", item.Item.HealthDiagnostic)
		}
	}
}
