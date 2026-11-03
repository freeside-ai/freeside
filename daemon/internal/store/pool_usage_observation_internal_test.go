package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var (
	observationAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	observedPool  = domain.UsagePoolID("pool-1")
)

// recordPoolEnrollment records an account-bound identity and one enrollment on
// it, at instant at.
func recordPoolEnrollment(
	t *testing.T, st *Store, identity domain.AuthIdentityID, enrollment domain.ClientEnrollmentID,
	pool string, at time.Time,
) {
	t.Helper()
	declared := enrollmentIdentity()
	declared.ID = identity
	declared.AccountBinding = "acct-" + string(identity)
	declared.UsagePool = pool
	record := codexEnrollment()
	record.ID = enrollment
	record.AuthIdentityID = identity
	record.AccountBinding = declared.AccountBinding
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		if err := tx.RecordAuthIdentity(t.Context(), declared, at); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(t.Context(), record, at)
	}); err != nil {
		t.Fatalf("record enrollment %s: %v", enrollment, err)
	}
}

// appendCollectorGeneration moves enroll-1 to its next generation under a lease
// of its own.
func appendCollectorGeneration(t *testing.T, st *Store, previous int, at time.Time) {
	t.Helper()
	ctx := t.Context()
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, "auth-1", "inv-refresh",
			&domain.LeaseGenerationBinding{
				EnrollmentID: "enroll-1", Generation: previous,
				AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
			}, at, at.Add(10*time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(lease.Fence), at.Add(time.Minute)); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-refresh", lease.Fence, at.Add(2*time.Minute))
	}); err != nil {
		t.Fatalf("append generation after %d: %v", previous, err)
	}
}

func openObservationStore(t *testing.T) *Store {
	t.Helper()
	st := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, st)
	appendCollectorGeneration(t, st, 0, observationAt.Add(-time.Hour))
	return st
}

func collectorReading(window string, generation int) domain.WindowReading {
	return domain.WindowReading{
		Window: window, EnrollmentID: "enroll-1", Generation: generation,
		Status: domain.WindowStatusAllowed, ObservedAt: observationAt,
	}
}

func collectedObservation(readings ...domain.WindowReading) domain.PoolUsageObservation {
	return domain.PoolUsageObservation{
		Pool: observedPool, Collector: "enroll-1",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: "enroll-1", Answer: domain.AvailabilitySupported, ObservedAt: observationAt},
		},
		Windows: readings,
	}
}

func writeObservation(st *Store, t *testing.T, observation domain.PoolUsageObservation, at time.Time) error {
	t.Helper()
	return st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordPoolUsageObservation(t.Context(), observation, at)
	})
}

func readObservation(st *Store, t *testing.T, pool domain.UsagePoolID) (domain.PoolUsageObservation, error) {
	t.Helper()
	var observation domain.PoolUsageObservation
	err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		observation, err = tx.GetPoolUsageObservation(t.Context(), pool)
		return err
	})
	return observation, err
}

func mustReadObservation(st *Store, t *testing.T, pool domain.UsagePoolID) domain.PoolUsageObservation {
	t.Helper()
	observation, err := readObservation(st, t, pool)
	if err != nil {
		t.Fatalf("read observation %s: %v", pool, err)
	}
	return observation
}

func writeRefusalMark(
	st *Store, t *testing.T, pool domain.UsagePoolID, mark domain.RefusalMark, at time.Time,
) error {
	t.Helper()
	return st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordWindowRefusalMark(t.Context(), pool, mark, at)
	})
}

// TestPoolUsageObservationRoundTrip stores each shape the goldens pin and reads
// it back whole, with exhausted state still derivable from what came back.
func TestPoolUsageObservationRoundTrip(t *testing.T) {
	t.Parallel()
	exhausted := collectorReading("seven_day_opus", 1)
	exhausted.Status = domain.WindowStatusExhausted
	exhausted.UsedPercent = new(100)
	exhausted.ResetsAt = new(observationAt.Add(72 * time.Hour))
	exhausted.Length = new(7 * 24 * time.Hour)

	collected := collectedObservation(collectorReading("five_hour", 1), exhausted)
	collected.Plan = new("max")
	collected.ModelWindows = []domain.ModelWindow{{RouteModelID: "model-a", Window: "seven_day_opus"}}
	collected.RefusalMarks = []domain.RefusalMark{{
		Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt,
	}}

	unsupported := collectedObservation()
	unsupported.LastAttempt.Status = domain.CollectionAttemptUnsupported
	unsupported.Enrollments[0].Answer = domain.AvailabilityUnsupported

	never := collectedObservation()
	never.LastAttempt = nil

	for name, observation := range map[string]domain.PoolUsageObservation{
		"collected": collected, "unsupported": unsupported, "never attempted": never,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openObservationStore(t)
			if err := writeObservation(st, t, observation, observationAt); err != nil {
				t.Fatalf("record: %v", err)
			}
			got := mustReadObservation(st, t, observedPool)
			if !reflect.DeepEqual(got, observation) {
				t.Fatalf("read back %+v, want %+v", got, observation)
			}
			if err := writeObservation(st, t, observation, observationAt); err != nil {
				t.Fatalf("replay at the same instant: %v", err)
			}
		})
	}
}

// A failed attempt keeps earlier readings. It cannot add one: that is a
// synthetic window the domain cannot see, on a collector that never answered.
func TestPoolUsageObservationFailedAttemptOnlyKeepsReadings(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	failed := func(readings ...domain.WindowReading) domain.PoolUsageObservation {
		observation := collectedObservation(readings...)
		observation.LastAttempt = &domain.CollectionAttempt{
			Status: domain.CollectionAttemptFailed, At: observationAt.Add(time.Hour), Detail: "provider returned 503",
		}
		return observation
	}

	err := writeObservation(st, t, failed(collectorReading("five_hour", 1)), observationAt)
	if !errors.Is(err, domain.ErrSyntheticWindow) {
		t.Fatalf("first attempt failed with a reading = %v, want %v", err, domain.ErrSyntheticWindow)
	}

	if err := writeObservation(st, t, collectedObservation(collectorReading("five_hour", 1)), observationAt); err != nil {
		t.Fatalf("collect: %v", err)
	}
	kept := failed(collectorReading("five_hour", 1))
	if err := writeObservation(st, t, kept, observationAt.Add(time.Hour)); err != nil {
		t.Fatalf("failed attempt keeping its reading: %v", err)
	}
	if got := mustReadObservation(st, t, observedPool); !reflect.DeepEqual(got, kept) {
		t.Fatalf("read back %+v, want %+v", got, kept)
	}

	err = writeObservation(st, t, failed(collectorReading("five_hour", 1), collectorReading("weekly", 1)),
		observationAt.Add(2*time.Hour))
	if !errors.Is(err, domain.ErrSyntheticWindow) {
		t.Fatalf("failed attempt adding a reading = %v, want %v", err, domain.ErrSyntheticWindow)
	}
}

func TestListPoolUsageObservations(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	recordPoolEnrollment(t, st, "auth-0", "enroll-0", "pool-0", observationAt)
	for _, observation := range []domain.PoolUsageObservation{
		collectedObservation(),
		{Pool: "pool-0", Collector: "enroll-0"},
	} {
		if err := writeObservation(st, t, observation, observationAt); err != nil {
			t.Fatalf("record %s: %v", observation.Pool, err)
		}
	}
	var listed []domain.PoolUsageObservation
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		listed, err = tx.ListPoolUsageObservations(t.Context())
		return err
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 || listed[0].Pool != "pool-0" || listed[1].Pool != observedPool {
		t.Fatalf("listed %+v, want pool-0 then %s", listed, observedPool)
	}
}

func TestGetPoolUsageObservationNotFound(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if _, err := readObservation(st, t, observedPool); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read = %v, want %v", err, ErrNotFound)
	}
}

func TestRecordPoolUsageObservationStaleWrite(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	revision := collectedObservation(collectorReading("five_hour", 1))
	if err := writeObservation(st, t, revision, observationAt); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := writeObservation(st, t, revision, observationAt.Add(-time.Second)); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("older revision = %v, want %v", err, ErrStaleWrite)
	}
	divergent := revision
	divergent.Plan = new("max")
	if err := writeObservation(st, t, divergent, observationAt); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("divergent revision at the same instant = %v, want %v", err, ErrStaleWrite)
	}
	if err := writeObservation(st, t, divergent, observationAt.Add(time.Second)); err != nil {
		t.Fatalf("later revision: %v", err)
	}
}

func TestRecordPoolUsageObservationRefusesInvalidRecord(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if err := writeObservation(st, t, collectedObservation(), time.Time{}); !errors.Is(err, domain.ErrMissingTimestamp) {
		t.Fatalf("zero recorded_at = %v, want %v", err, domain.ErrMissingTimestamp)
	}
	synthetic := collectedObservation(collectorReading("five_hour", 1))
	synthetic.LastAttempt = nil
	if err := writeObservation(st, t, synthetic, observationAt); !errors.Is(err, domain.ErrSyntheticWindow) {
		t.Fatalf("synthetic window = %v, want %v", err, domain.ErrSyntheticWindow)
	}
}

func TestPoolUsageObservationEnrollmentMustBeOnPool(t *testing.T) {
	t.Parallel()

	t.Run("refused on write", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		recordPoolEnrollment(t, st, "auth-2", "enroll-2", "pool-2", observationAt)

		foreignCollector := collectedObservation()
		foreignCollector.Collector = "enroll-2"
		if err := writeObservation(st, t, foreignCollector, observationAt); !errors.Is(err, ErrEnrollmentNotOnPool) {
			t.Fatalf("foreign collector = %v, want %v", err, ErrEnrollmentNotOnPool)
		}

		foreignAnswer := collectedObservation()
		foreignAnswer.Enrollments = append(foreignAnswer.Enrollments, domain.EnrollmentAvailability{
			EnrollmentID: "enroll-2", Answer: domain.AvailabilitySupported, ObservedAt: observationAt,
		})
		if err := writeObservation(st, t, foreignAnswer, observationAt); !errors.Is(err, ErrEnrollmentNotOnPool) {
			t.Fatalf("foreign availability = %v, want %v", err, ErrEnrollmentNotOnPool)
		}

		unknown := collectedObservation()
		unknown.Collector = "enroll-missing"
		if err := writeObservation(st, t, unknown, observationAt); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown collector = %v, want %v", err, ErrNotFound)
		}
	})

	// A body is never read back on its own authority: both rows take a tampered
	// body, because characterization removes the only record that could go stale
	// by itself (TestCharacterizationRetiresTheDerivedPoolRecord).
	t.Run("refused on read", func(t *testing.T) {
		t.Parallel()
		for name, statement := range map[string]string{
			"collector": `UPDATE pool_usage_observations SET collector = 'enroll-2',
				body = json_set(body, '$.observation.collector', 'enroll-2')`,
			"availability": `UPDATE pool_usage_observations
				SET body = json_set(body, '$.observation.enrollments[0].enrollment_id', 'enroll-2')`,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				st := openObservationStore(t)
				recordPoolEnrollment(t, st, "auth-2", "enroll-2", "pool-2", observationAt)
				if err := writeObservation(st, t, collectedObservation(), observationAt); err != nil {
					t.Fatalf("record: %v", err)
				}
				if _, err := st.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatalf("tamper: %v", err)
				}
				if _, err := readObservation(st, t, observedPool); !errors.Is(err, ErrEnrollmentNotOnPool) {
					t.Fatalf("read = %v, want %v", err, ErrEnrollmentNotOnPool)
				}
				if err := st.Read(t.Context(), func(tx *ReadTx) error {
					_, err := tx.ListPoolUsageObservations(t.Context())
					return err
				}); !errors.Is(err, ErrEnrollmentNotOnPool) {
					t.Fatalf("list = %v, want %v", err, ErrEnrollmentNotOnPool)
				}
			})
		}
	})
}

// An identity with no pool yet meters its derived one. Characterizing it into a
// shared pool leaves that pool with no enrollment, so its record goes with it
// and the listing keeps working for every other pool.
func TestCharacterizationRetiresTheDerivedPoolRecord(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if err := writeObservation(st, t, collectedObservation(), observationAt); err != nil {
		t.Fatalf("record the shared pool: %v", err)
	}
	recordPoolEnrollment(t, st, "auth-moves", "enroll-moves", "", observationAt)
	moved := enrollmentIdentity()
	moved.ID = "auth-moves"
	moved.AccountBinding = "acct-auth-moves"
	moved.UsagePool = ""
	derived := moved.EffectiveUsagePool()
	if err := writeObservation(st, t,
		domain.PoolUsageObservation{Pool: derived, Collector: "enroll-moves"}, observationAt); err != nil {
		t.Fatalf("record the derived pool: %v", err)
	}

	moved.UsagePool = string(observedPool)
	recordSlotIdentity(t, st, moved, observationAt.Add(time.Hour))

	if _, err := readObservation(st, t, derived); !errors.Is(err, ErrNotFound) {
		t.Fatalf("derived pool's record = %v, want %v", err, ErrNotFound)
	}
	var listed []domain.PoolUsageObservation
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		listed, err = tx.ListPoolUsageObservations(t.Context())
		return err
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Pool != observedPool {
		t.Fatalf("listed %+v, want the shared pool alone", listed)
	}
}

// A body naming a generation the collector never had fails closed, and reads
// as an inconsistent row rather than as a pool with no record.
func TestGetPoolUsageObservationRefusesAForgedGeneration(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if err := writeObservation(st, t, collectedObservation(collectorReading("five_hour", 1)), observationAt); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := st.db.ExecContext(t.Context(), `UPDATE pool_usage_observations
		SET body = json_set(body, '$.observation.windows[0].generation', 99)`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err := readObservation(st, t, observedPool)
	if !errors.Is(err, errRowInconsistent) || errors.Is(err, ErrNotFound) {
		t.Fatalf("read = %v, want %v and not %v", err, errRowInconsistent, ErrNotFound)
	}
}

// An allowed reading clears a mark for good: the reading that later replaces it
// has no mark to revive, even once that reading's own reset has passed.
func TestRecordPoolUsageObservationDropsClearedMarks(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if err := writeRefusalMark(st, t, observedPool, domain.RefusalMark{
		Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt.Add(-time.Hour),
	}, observationAt.Add(-time.Hour)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := writeObservation(st, t, collectedObservation(collectorReading("five_hour", 1)), observationAt); err != nil {
		t.Fatalf("allowed reading: %v", err)
	}
	cleared := mustReadObservation(st, t, observedPool)
	if len(cleared.RefusalMarks) != 0 {
		t.Fatalf("marks after the allowed reading = %+v, want none", cleared.RefusalMarks)
	}

	resetsAt := observationAt.Add(2 * time.Hour)
	exhausted := collectorReading("five_hour", 1)
	exhausted.Status = domain.WindowStatusExhausted
	exhausted.ResetsAt = &resetsAt
	exhausted.ObservedAt = observationAt.Add(time.Hour)
	if err := writeObservation(st, t, collectedObservation(exhausted), observationAt.Add(time.Hour)); err != nil {
		t.Fatalf("exhausted reading: %v", err)
	}
	if marks := mustReadObservation(st, t, observedPool).ExhaustedMarks(resetsAt); len(marks) != 0 {
		t.Fatalf("marks after the reading's reset = %+v, want none", marks)
	}
}

// A reading observed earlier must not undo a later one, whatever instant the
// late write is recorded at.
func TestRecordPoolUsageObservationRefusesAnOlderReading(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	exhausted := collectorReading("five_hour", 1)
	exhausted.Status = domain.WindowStatusExhausted
	if err := writeObservation(st, t, collectedObservation(exhausted), observationAt); err != nil {
		t.Fatalf("record: %v", err)
	}
	older := collectorReading("five_hour", 1)
	older.ObservedAt = observationAt.Add(-time.Hour)
	err := writeObservation(st, t, collectedObservation(older), observationAt.Add(time.Hour))
	if !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("older reading = %v, want %v", err, ErrStaleWrite)
	}
}

func TestPoolUsageObservationGenerationGate(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	first := collectorReading("five_hour", 1)
	if err := writeObservation(st, t, collectedObservation(first), observationAt); err != nil {
		t.Fatalf("record at generation 1: %v", err)
	}
	appendCollectorGeneration(t, st, 1, observationAt.Add(time.Minute))

	replaced := first
	replaced.ObservedAt = observationAt.Add(time.Hour)
	err := writeObservation(st, t, collectedObservation(replaced), observationAt.Add(time.Hour))
	if !errors.Is(err, ErrReadingGenerationReplaced) {
		t.Fatalf("new reading at generation 1 = %v, want %v", err, ErrReadingGenerationReplaced)
	}

	// A write that keeps the reading is not a new reading: a partial update, a
	// failed refresh, or a refusal mark all rewrite the record under the later
	// generation, and the kept reading goes on naming the one it was taken under.
	if err := writeRefusalMark(st, t, observedPool, domain.RefusalMark{
		Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt.Add(time.Hour),
	}, observationAt.Add(time.Hour)); err != nil {
		t.Fatalf("refusal mark after a later generation: %v", err)
	}
	partial := mustReadObservation(st, t, observedPool)
	partial.Windows = append(partial.Windows, collectorReading("weekly", 2))
	if err := writeObservation(st, t, partial, observationAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("partial update beside a kept reading: %v", err)
	}

	got := mustReadObservation(st, t, observedPool)
	if len(got.Windows) != 2 || got.Windows[0].Generation != 1 || got.Windows[1].Generation != 2 {
		t.Fatalf("windows = %+v, want five_hour at 1 and weekly at 2", got.Windows)
	}
}

func TestRecordPoolUsageObservationReadingNeedsAGeneration(t *testing.T) {
	t.Parallel()
	st := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, st)
	err := writeObservation(st, t, collectedObservation(collectorReading("five_hour", 1)), observationAt)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading from a collector with no generation = %v, want %v", err, ErrNotFound)
	}
}

func TestRecordWindowRefusalMark(t *testing.T) {
	t.Parallel()
	mark := func(window string, invocation domain.InvocationID) domain.RefusalMark {
		return domain.RefusalMark{
			Window: window, InvocationID: invocation, ResetsAt: new(observationAt.Add(time.Hour)), SetAt: observationAt,
		}
	}

	t.Run("on a pool with a record", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		collected := collectedObservation(collectorReading("weekly", 1))
		collected.Plan = new("max")
		if err := writeObservation(st, t, collected, observationAt); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := writeRefusalMark(st, t, observedPool, mark("weekly", "inv-1"), observationAt.Add(time.Minute)); err != nil {
			t.Fatalf("mark weekly: %v", err)
		}
		if err := writeRefusalMark(st, t, observedPool, mark("five_hour", "inv-2"), observationAt.Add(2*time.Minute)); err != nil {
			t.Fatalf("mark five_hour: %v", err)
		}
		if err := writeRefusalMark(st, t, observedPool, mark("weekly", "inv-3"), observationAt.Add(3*time.Minute)); err != nil {
			t.Fatalf("replace weekly: %v", err)
		}

		got := mustReadObservation(st, t, observedPool)
		want := collected
		want.RefusalMarks = []domain.RefusalMark{mark("five_hour", "inv-2"), mark("weekly", "inv-3")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back %+v, want %+v", got, want)
		}
		// weekly's allowed reading is not after its mark, so it clears nothing.
		if marks := got.ExhaustedMarks(observationAt); len(marks) != 2 {
			t.Fatalf("marks = %+v, want five_hour and weekly", marks)
		}
	})

	t.Run("on a pool without a record", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		if err := writeRefusalMark(st, t, observedPool, mark("five_hour", "inv-1"), observationAt); err != nil {
			t.Fatalf("mark: %v", err)
		}
		want := domain.PoolUsageObservation{
			Pool: observedPool, Collector: "enroll-1",
			RefusalMarks: []domain.RefusalMark{mark("five_hour", "inv-1")},
		}
		if got := mustReadObservation(st, t, observedPool); !reflect.DeepEqual(got, want) {
			t.Fatalf("read back %+v, want %+v", got, want)
		}
	})

	// The read and the write share a transaction, so neither a shared instant
	// nor a recordedAt behind the stored revision can lose a provider refusal.
	t.Run("kept at any instant", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		if err := writeRefusalMark(st, t, observedPool, mark("five_hour", "inv-1"), observationAt); err != nil {
			t.Fatalf("mark: %v", err)
		}
		if err := writeRefusalMark(st, t, observedPool, mark("weekly", "inv-2"), observationAt); err != nil {
			t.Fatalf("mark at the same instant: %v", err)
		}
		if err := writeRefusalMark(st, t, observedPool, mark("monthly", "inv-3"), observationAt.Add(-time.Second)); err != nil {
			t.Fatalf("mark behind the stored revision: %v", err)
		}
		if got := mustReadObservation(st, t, observedPool).RefusalMarks; len(got) != 3 {
			t.Fatalf("marks = %+v, want three", got)
		}
		// The revision kept its instant, so an older whole record stays stale.
		older := collectedObservation()
		if err := writeObservation(st, t, older, observationAt.Add(-time.Second)); !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("older record = %v, want %v", err, ErrStaleWrite)
		}
	})

	t.Run("an older mark keeps the newer one", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		if err := writeRefusalMark(st, t, observedPool, mark("five_hour", "inv-1"), observationAt); err != nil {
			t.Fatalf("mark: %v", err)
		}
		delayed := mark("five_hour", "inv-2")
		delayed.SetAt = observationAt.Add(-time.Minute)
		if err := writeRefusalMark(st, t, observedPool, delayed, observationAt.Add(time.Minute)); err != nil {
			t.Fatalf("delayed mark: %v", err)
		}
		if got := mustReadObservation(st, t, observedPool).RefusalMarks[0].InvocationID; got != "inv-1" {
			t.Fatalf("holder = %s, want inv-1", got)
		}
	})

	t.Run("refuses a zero instant", func(t *testing.T) {
		t.Parallel()
		st := openObservationStore(t)
		err := writeRefusalMark(st, t, observedPool, mark("five_hour", "inv-1"), time.Time{})
		if !errors.Is(err, domain.ErrMissingTimestamp) {
			t.Fatalf("mark = %v, want %v", err, domain.ErrMissingTimestamp)
		}
	})

	t.Run("on a pool with no enrollment", func(t *testing.T) {
		t.Parallel()
		st := openSlotStore(t)
		recordSlotIdentity(t, st, slotIdentity("auth-bare", "pool-bare"), slotAt)
		err := writeRefusalMark(st, t, "pool-bare", mark("five_hour", "inv-1"), observationAt)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("mark = %v, want %v", err, ErrNotFound)
		}
	})
}

// The first enrollment is the earliest recorded, across every identity on the
// pool, whatever order the ids sort in.
func TestDefaultPoolCollector(t *testing.T) {
	t.Parallel()
	st := openEnrollmentStore(t)
	collector := func(t *testing.T, pool domain.UsagePoolID) (domain.ClientEnrollmentID, error) {
		t.Helper()
		var id domain.ClientEnrollmentID
		err := st.Read(t.Context(), func(tx *ReadTx) error {
			var err error
			id, err = tx.DefaultPoolCollector(t.Context(), pool)
			return err
		})
		return id, err
	}
	if _, err := collector(t, "pool-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pool with no enrollment = %v, want %v", err, ErrNotFound)
	}

	recordPoolEnrollment(t, st, "auth-z", "enroll-z", "pool-1", observationAt)
	recordPoolEnrollment(t, st, "auth-a", "enroll-a", "pool-1", observationAt.Add(time.Hour))
	recordPoolEnrollment(t, st, "auth-other", "enroll-0", "pool-2", observationAt.Add(-time.Hour))
	if got, err := collector(t, "pool-1"); err != nil || got != "enroll-z" {
		t.Fatalf("default collector = %q, %v, want enroll-z", got, err)
	}

	recordPoolEnrollment(t, st, "auth-tie", "enroll-tie", "pool-1", observationAt)
	if got, err := collector(t, "pool-1"); err != nil || got != "enroll-tie" {
		t.Fatalf("default collector on a tie = %q, %v, want the lower id enroll-tie", got, err)
	}
}

// A row whose columns disagree with its body fails closed, as every extracted
// record does.
func TestGetPoolUsageObservationCrossChecksColumns(t *testing.T) {
	t.Parallel()
	for name, statement := range map[string]string{
		"collector":      `UPDATE pool_usage_observations SET collector = 'enroll-2'`,
		"attempt status": `UPDATE pool_usage_observations SET last_attempt_status = 'failed'`,
		"attempt time":   `UPDATE pool_usage_observations SET last_attempt_at = '2026-01-01T00:00:00Z'`,
		"attempt absent": `UPDATE pool_usage_observations SET last_attempt_status = NULL, last_attempt_at = NULL`,
		"recorded at":    `UPDATE pool_usage_observations SET recorded_at = '2026-01-01T00:00:00Z'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openObservationStore(t)
			recordPoolEnrollment(t, st, "auth-2", "enroll-2", string(observedPool), observationAt)
			if err := writeObservation(st, t, collectedObservation(), observationAt); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := st.db.ExecContext(t.Context(), statement); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if _, err := readObservation(st, t, observedPool); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

// Clients sync the record (#1719), so a record write and a refusal mark each
// raise the revision on their own, stamp it into the row, and advance the
// row's version.
func TestPoolUsageObservationWritesRaiseRevision(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	revision := func() int64 {
		t.Helper()
		state, err := st.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return state.Revision
	}
	snapshot := func() Snapshot {
		t.Helper()
		var snapshotted Snapshotted[domain.PoolUsageObservation]
		if err := st.Read(t.Context(), func(tx *ReadTx) error {
			var err error
			snapshotted, err = tx.GetPoolUsageObservationSnapshot(t.Context(), observedPool)
			return err
		}); err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		return snapshotted.Snapshot
	}

	before := revision()
	if err := writeObservation(st, t, collectedObservation(collectorReading("five_hour", 1)), observationAt); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := revision(); got != before+1 {
		t.Fatalf("revision after record = %d, want %d", got, before+1)
	}
	if got := snapshot(); got != (Snapshot{AsOfRevision: before + 1, EntityVersion: 1}) {
		t.Fatalf("snapshot after record = %+v", got)
	}

	mark := domain.RefusalMark{Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt.Add(time.Minute)}
	if err := writeRefusalMark(st, t, observedPool, mark, observationAt.Add(time.Minute)); err != nil {
		t.Fatalf("refusal mark: %v", err)
	}
	if got := revision(); got != before+2 {
		t.Fatalf("revision after refusal mark = %d, want %d", got, before+2)
	}
	if got := snapshot(); got != (Snapshot{AsOfRevision: before + 2, EntityVersion: 2}) {
		t.Fatalf("snapshot after refusal mark = %+v", got)
	}
}

// A row with no sync metadata is one no writer produced, so it fails closed.
func TestGetPoolUsageObservationRequiresSyncMetadata(t *testing.T) {
	t.Parallel()
	for name, statement := range map[string]string{
		"as_of_revision": `UPDATE pool_usage_observations SET as_of_revision = 0`,
		"entity_version": `UPDATE pool_usage_observations SET entity_version = 0`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openObservationStore(t)
			if err := writeObservation(st, t, collectedObservation(), observationAt); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := st.db.ExecContext(t.Context(), statement); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if _, err := readObservation(st, t, observedPool); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

// RequireEnrollmentOnPool names the two ways a reassignment target fails.
func TestRequireEnrollmentOnPool(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	recordPoolEnrollment(t, st, "auth-other", "enroll-other", "pool-2", observationAt)
	check := func(id domain.ClientEnrollmentID) error {
		return st.Read(t.Context(), func(tx *ReadTx) error {
			return tx.RequireEnrollmentOnPool(t.Context(), observedPool, id)
		})
	}
	if err := check("enroll-1"); err != nil {
		t.Fatalf("collector on the pool = %v", err)
	}
	if err := check("enroll-other"); !errors.Is(err, ErrEnrollmentNotOnPool) {
		t.Fatalf("enrollment on another pool = %v, want %v", err, ErrEnrollmentNotOnPool)
	}
	if err := check("enroll-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing enrollment = %v, want %v", err, ErrNotFound)
	}
}
