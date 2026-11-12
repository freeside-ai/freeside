package engine

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	drainOldBuild domain.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	drainNewBuild domain.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

var drainEpoch = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// drainLogRow is one row of the update drain log, read raw so a test can see
// a lifted row's build, which no store read returns.
type drainLogRow struct {
	State domain.UpdateDrainState
	Build domain.Digest
}

func readDrainLog(t *testing.T, dbPath string) []drainLogRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	rows, err := db.QueryContext(t.Context(), `SELECT state, build FROM update_drain_transitions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test cleanup
	log := []drainLogRow{}
	for rows.Next() {
		var row drainLogRow
		if err := rows.Scan(&row.State, &row.Build); err != nil {
			t.Fatal(err)
		}
		log = append(log, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return log
}

func drainRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var revision int64
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		state, err := tx.ServerState(t.Context())
		revision = state.Revision
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}

func drainState(t *testing.T, st *store.Store) *domain.UpdateDrainTransition {
	t.Helper()
	var drain *domain.UpdateDrainTransition
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		drain, err = tx.UpdateDrainInForce(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return drain
}

func newDrain(st *store.Store, build domain.Digest, now *time.Time) UpdateDrain {
	return UpdateDrain{Store: st, Build: build, Now: func() time.Time { return *now }}
}

// TestUpdateDrainRequestKeepsTheDrainInForce: a request writes one draining
// row and moves the revision; a second request writes nothing and returns
// the first drain, so its start time stays.
func TestUpdateDrainRequestKeepsTheDrainInForce(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	now := drainEpoch
	drain := newDrain(st, drainOldBuild, &now)

	before := drainRevision(t, st)
	first, err := drain.Request(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := domain.UpdateDrainTransition{State: domain.UpdateDrainDraining, Build: drainOldBuild, OccurredAt: drainEpoch}
	if first != want {
		t.Fatalf("request = %+v, want %+v", first, want)
	}
	requested := drainRevision(t, st)
	if requested <= before {
		t.Fatalf("revision after request = %d, want past %d", requested, before)
	}
	now = drainEpoch.Add(time.Minute)
	second, err := drain.Request(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second != want {
		t.Fatalf("second request = %+v, want the first drain %+v", second, want)
	}
	if got := drainRevision(t, st); got != requested {
		t.Fatalf("revision after a repeated request = %d, want %d", got, requested)
	}
	if log := readDrainLog(t, dbPath); len(log) != 1 {
		t.Fatalf("drain log = %+v, want one row", log)
	}
}

// TestUpdateDrainCancelLiftsOnlyADrainInForce: a cancel with no drain writes
// nothing; with one in force it lifts it, moves the revision, and ends every
// run's update_drain hold.
func TestUpdateDrainCancelLiftsOnlyADrainInForce(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-drained")
	inv := domain.InvocationID("inv-drained")
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			return err
		}
		return tx.RecordRunHold(t.Context(), domain.RunHoldObservation{
			RunID: runID, InvocationID: &inv, Reason: domain.HoldUpdateDrain,
			FirstObservedAt: drainEpoch, LastObservedAt: drainEpoch,
		})
	}); err != nil {
		t.Fatal(err)
	}
	now := drainEpoch
	drain := newDrain(st, drainOldBuild, &now)

	before := drainRevision(t, st)
	lifted, err := drain.Cancel(t.Context())
	if err != nil || lifted {
		t.Fatalf("cancel with no drain = %v, %v; want false, nil", lifted, err)
	}
	if got := drainRevision(t, st); got != before {
		t.Fatalf("revision after an empty cancel = %d, want %d", got, before)
	}
	if _, err := drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	requested := drainRevision(t, st)
	lifted, err = drain.Cancel(t.Context())
	if err != nil || !lifted {
		t.Fatalf("cancel = %v, %v; want true, nil", lifted, err)
	}
	if got := drainState(t, st); got != nil {
		t.Fatalf("drain after cancel = %+v, want none", got)
	}
	if got := drainRevision(t, st); got <= requested {
		t.Fatalf("revision after cancel = %d, want past %d", got, requested)
	}
	if hold := storedRunHold(t, st, runID); hold != nil {
		t.Fatalf("hold after cancel = %+v, want none", hold)
	}
}

// TestUpdateDrainLiftForBuild: the build that requested a drain leaves it in
// force; another build lifts it with a lifted row naming that build.
func TestUpdateDrainLiftForBuild(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	now := drainEpoch
	// Each recorded transition moves the sync revision, so a client's
	// heartbeat re-bootstraps and sees the drain stop appear or go.
	before := serverRevision(t, st)
	if _, err := newDrain(st, drainOldBuild, &now).Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	requested := serverRevision(t, st)
	if requested <= before {
		t.Fatalf("the request left the revision at %d", requested)
	}
	lifted, err := newDrain(st, drainOldBuild, &now).LiftForBuild(t.Context())
	if err != nil || lifted {
		t.Fatalf("same-build lift = %v, %v; want false, nil", lifted, err)
	}
	if drainState(t, st) == nil {
		t.Fatal("the same build lifted its own drain")
	}
	lifted, err = newDrain(st, drainNewBuild, &now).LiftForBuild(t.Context())
	if err != nil || !lifted {
		t.Fatalf("new-build lift = %v, %v; want true, nil", lifted, err)
	}
	if after := serverRevision(t, st); after <= requested {
		t.Fatalf("the lift left the revision at %d", after)
	}
	want := []drainLogRow{
		{State: domain.UpdateDrainDraining, Build: drainOldBuild},
		{State: domain.UpdateDrainLifted, Build: drainNewBuild},
	}
	if log := readDrainLog(t, dbPath); !reflect.DeepEqual(log, want) {
		t.Fatalf("drain log = %+v, want %+v", log, want)
	}
	lifted, err = newDrain(st, drainNewBuild, &now).LiftForBuild(t.Context())
	if err != nil || lifted {
		t.Fatalf("lift with no drain = %v, %v; want false, nil", lifted, err)
	}
}

// TestUpdateDrainTransitionsSurviveACrashOnEitherSide models process loss at
// each drain transition's seams against a reopened store: before the write
// nothing is recorded and a repeat records it; after the write the row is
// there and a repeat records nothing more.
func TestUpdateDrainTransitionsSurviveACrashOnEitherSide(t *testing.T) {
	t.Parallel()
	type step struct {
		transition DurableTransition
		// setup puts the log in the state the transition starts from.
		setup func(UpdateDrain) error
		run   func(UpdateDrain) error
		// build is the build that runs the transition.
		build domain.Digest
		want  []drainLogRow
	}
	request := func(d UpdateDrain) error { _, err := d.Request(t.Context()); return err }
	cancel := func(d UpdateDrain) error { _, err := d.Cancel(t.Context()); return err }
	lift := func(d UpdateDrain) error { _, err := d.LiftForBuild(t.Context()); return err }
	none := func(UpdateDrain) error { return nil }
	drained := []drainLogRow{{State: domain.UpdateDrainDraining, Build: drainOldBuild}}
	steps := []step{
		{DurableTransitionUpdateDrainRequest, none, request, drainOldBuild, drained},
		{
			DurableTransitionUpdateDrainCancel, request, cancel, drainOldBuild,
			append(drained[:1:1], drainLogRow{State: domain.UpdateDrainLifted, Build: drainOldBuild}),
		},
		{
			DurableTransitionUpdateDrainLift, request, lift, drainNewBuild,
			append(drained[:1:1], drainLogRow{State: domain.UpdateDrainLifted, Build: drainNewBuild}),
		},
	}
	for _, s := range steps {
		for _, side := range []DurableTransitionSide{DurableTransitionBefore, DurableTransitionAfter} {
			t.Run(string(s.transition)+"/"+string(side), func(t *testing.T) {
				t.Parallel()
				dbPath := filepath.Join(t.TempDir(), "freeside.db")
				storetest.Copy(t, dbPath)
				crashed, err := store.Open(t.Context(), dbPath, store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				now := drainEpoch
				if err := s.setup(newDrain(crashed, drainOldBuild, &now)); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("injected process loss")
				drain := newDrain(crashed, s.build, &now)
				drain.TransitionHook = func(transition DurableTransition, at DurableTransitionSide) error {
					if transition == s.transition && at == side {
						return injected
					}
					return nil
				}
				if err := s.run(drain); !errors.Is(err, injected) {
					t.Fatalf("crash at %s = %v, want the injected loss", side, err)
				}
				if err := crashed.Close(); err != nil {
					t.Fatal(err)
				}
				wantAtCrash := s.want[:len(s.want)-1]
				if side == DurableTransitionAfter {
					wantAtCrash = s.want
				}
				if log := readDrainLog(t, dbPath); !reflect.DeepEqual(log, wantAtCrash) {
					t.Fatalf("log at the crash = %+v, want %+v", log, wantAtCrash)
				}
				reopened := storetest.Open(t, dbPath, store.Options{})
				if err := s.run(newDrain(reopened, s.build, &now)); err != nil {
					t.Fatalf("repeat after reopen: %v", err)
				}
				if log := readDrainLog(t, dbPath); !reflect.DeepEqual(log, s.want) {
					t.Fatalf("log after the repeat = %+v, want %+v", log, s.want)
				}
			})
		}
	}
}

// TestRunningWorkCountsUntilEnd: Begin counts a kind until its end, a second
// end changes nothing, and a nil counter counts nothing.
func TestRunningWorkCountsUntilEnd(t *testing.T) {
	t.Parallel()
	running := NewRunningWork()
	first := running.Begin(domain.RestartBlockerProbe)
	second := running.Begin(domain.RestartBlockerProbe)
	if got := running.Counts(); got[domain.RestartBlockerProbe] != 2 {
		t.Fatalf("counts = %v, want two probes", got)
	}
	first()
	first()
	if got := running.Counts(); got[domain.RestartBlockerProbe] != 1 {
		t.Fatalf("counts after one end twice = %v, want one probe", got)
	}
	second()
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after every end = %v, want none", got)
	}
	var unset *RunningWork
	unset.Begin(domain.RestartBlockerProbe)()
	if got := unset.Counts(); got != nil {
		t.Fatalf("nil counter counts = %v, want nil", got)
	}
}

// TestBeginUnlessDrainingEndsTheCountOnRefusal: while a drain is in force the
// check refuses with the sentinel and leaves nothing counted.
func TestBeginUnlessDrainingEndsTheCountOnRefusal(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	running := NewRunningWork()
	end, err := running.BeginUnlessDraining(t.Context(), st, domain.RestartBlockerImageBuild)
	if err != nil {
		t.Fatal(err)
	}
	if got := running.Counts(); got[domain.RestartBlockerImageBuild] != 1 {
		t.Fatalf("counts = %v, want one image build", got)
	}
	end()
	now := drainEpoch
	if _, err := newDrain(st, drainOldBuild, &now).Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := running.BeginUnlessDraining(t.Context(), st, domain.RestartBlockerImageBuild); !errors.Is(
		err, store.ErrUpdateDrainInForce) {
		t.Fatalf("begin while draining = %v, want ErrUpdateDrainInForce", err)
	}
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after a refusal = %v, want none", got)
	}
}
