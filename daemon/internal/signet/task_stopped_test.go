package signet_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestConfirmedCancellationProjectsTerminalTaskAndReleasesOnlyHeldSlot(t *testing.T) {
	for _, phase := range []string{"queued", "no-artifact", "spec-approval", "specification", "implementation", "review", "verification"} {
		t.Run(phase, func(t *testing.T) {
			f := newRunFixture(t)
			var phases []string
			if phase != "queued" {
				phases = []string{phase}
			}
			id := seedStopTask(t, f.store, phases...)
			if len(phases) > 0 {
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
					return tx.RecordTaskStart(t.Context(), domain.RunID("run-"+phase), time.Now().UTC())
				}); err != nil {
					t.Fatal(err)
				}
			}
			read := func() signet.Task {
				t.Helper()
				b, err := f.service.Bootstrap(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range b.Tasks {
					if s.Task.ID == id {
						return s.Task
					}
				}
				t.Fatal("missing task")
				return signet.Task{}
			}
			before := read()
			command := stopCommand(t, f.store, id, "stop")
			accepted, err := f.service.Submit(t.Context(), command)
			if err != nil {
				t.Fatal(err)
			}
			if got := read(); got.WIP != before.WIP || !reflect.DeepEqual(got.Lifecycle, before.Lifecycle) {
				t.Fatal("acceptance changed lifecycle or WIP")
			}
			c := accepted.Stop.Cancellation
			ack := domain.TaskCancellationAcknowledgement{RequestID: c.RequestID, TargetDigest: c.TargetDigest, EvidenceDigest: c.TargetDigest}
			for i, state := range []domain.TaskCancellationState{domain.TaskCancellationFailed, domain.TaskCancellationConfirmed} {
				ack.ID, ack.State, ack.RecordedAt = string(state), state, before.LastActivityAt.Add(time.Duration(i+1)*time.Second)
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); err != nil {
					t.Fatal(err)
				}
				got := read()
				if state == domain.TaskCancellationFailed {
					if got.WIP != before.WIP || !reflect.DeepEqual(got.Lifecycle, before.Lifecycle) {
						t.Fatal("failed stop released slot or marked stopped")
					}
					continue
				}
				if got.WIP || got.Lifecycle == nil || *got.Lifecycle != domain.TaskStopped || got.LastActivityAt != ack.RecordedAt {
					t.Fatalf("confirmed projection: %+v", got)
				}
				wantFacts := 0
				if phase != "queued" {
					wantFacts = 2
				}
				if len(got.LifecycleFacts) != wantFacts || !slices.Equal(got.RunIDs, before.RunIDs) || !reflect.DeepEqual(got.CurrentPosition, before.CurrentPosition) || got.DisplayNames != before.DisplayNames {
					t.Fatalf("confirmation changed task history: %+v", got)
				}
				if wantFacts > 0 && got.LifecycleFacts[1].Kind != domain.TaskLifecycleAbandoned {
					t.Fatal("held episode was not abandoned")
				}
			}
			terminal := read()
			beforeRevision, _ := f.store.ServerState(t.Context())
			rollback := errors.New("no change")
			if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				changed, err := tx.AcknowledgeTaskCancellation(t.Context(), ack)
				if err != nil {
					return err
				}
				if changed {
					t.Fatal("replay changed ack")
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			afterRevision, _ := f.store.ServerState(t.Context())
			if beforeRevision != afterRevision || !reflect.DeepEqual(terminal, read()) {
				t.Fatal("replay changed facts or revision")
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store = storetest.Open(t, f.dbPath, store.Options{})
			t.Cleanup(func() { _ = f.store.Close() })
			f.service = signet.NewService(f.store)
			if !reflect.DeepEqual(terminal, read()) {
				t.Fatal("restart changed terminal task")
			}
			replay, err := f.service.Submit(t.Context(), command)
			if err != nil || !reflect.DeepEqual(replay, accepted) {
				t.Fatalf("receipt replay: %v", err)
			}
		})
	}
}
