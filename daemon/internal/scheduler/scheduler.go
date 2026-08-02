// Package scheduler is the §5.16 durable scheduler: one component owns
// every durable deferred check — PR watches, deadlines, subject-bound
// polls — as fires over the closed domain.ScheduleKind union. It performs
// fire-time validation (operating-mode eligibility, expiry, activation
// state, subject existence, project binding, and resolved-policy authority)
// before constructing a trusted domain.ScheduleEvent. It coalesces missed
// fires to the latest nominal occurrence with a recorded gap, and commits
// each occurrence's consumption and its handler's durable outcome in one
// store transaction, so a crashed
// or failed consumption leaves the occurrence durably pending for
// redelivery.
//
// The scheduler owns cadence and delivery only. Handlers own meaning: what
// a fire does, and whether a stale event re-arms (a new generation with a
// corrected binding) or records proof the condition no longer applies.
// Firing never extends or preserves authority — an event carries identity
// and expectations, and every credential a handler needs comes from its own
// composition, never from the scheduler.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrStaleConsumption abandons a consumption from inside its committing
// transaction: a handler's Commit returns it when the transactional re-read
// contradicts the decision the handler computed outside the transaction (an
// operator concluded the item after the handler's read, say). The whole
// transaction rolls back, the occurrence stays durably pending, and the
// next pass revalidates the fire against the state that won the
// serialization — never an error, and never a silently recorded stale
// outcome.
var ErrStaleConsumption = errors.New("scheduler: consumption is stale; redeliver")

// Consumption is a handler's durable outcome for one fired occurrence. The
// scheduler commits it atomically with the occurrence's consumed mark:
// Schedule (when non-nil) replaces the aggregate — a conclusion or a
// re-arm — and Commit (when non-nil) applies any further synced outcome
// (an item fact) inside the same transaction. Commit may return
// ErrStaleConsumption to abandon the consumption entirely.
type Consumption struct {
	Outcome  domain.ScheduleOccurrenceOutcome
	Schedule *domain.Schedule
	Commit   func(context.Context, *store.WriteTx) error
}

// Handler consumes one fired occurrence. Long or external work (a janitor
// cycle, a doctor pass, a GitHub read) runs here, outside any store
// transaction; the returned Consumption is what commits. A returned error
// is a correctness failure and stops the scheduler — a transient external
// failure is an OutcomeObserveFailed consumption, not an error, so a
// recurring schedule retries at its next nominal fire.
type Handler func(ctx context.Context, ev domain.ScheduleEvent, s domain.Schedule) (Consumption, error)

// Registration wires one schedule kind: its handler, and (for kinds whose
// subject the scheduler cannot read from the store) the subject
// liveness/activation check. For attention_item kinds a nil SubjectLive
// selects the built-in open-item check; for trusted_config kinds the
// subject is the daemon's own configuration and is always live;
// installation_poll must supply the §5.9 activation-state check against the
// authority document.
type Registration struct {
	Handle      Handler
	SubjectLive func(ctx context.Context, s domain.Schedule) (bool, error)
}

// Scheduler drives the durable schedules of its registered kinds. Kinds
// without a registration are left untouched, so a composition that owns
// only one kind (the onboarding CLI's installation poll) can run beside a
// store carrying others.
type Scheduler struct {
	store *store.Store
	mode  domain.OperatingMode
	clock func() time.Time
	kinds map[domain.ScheduleKind]Registration

	mu       sync.Mutex
	inFlight map[domain.ScheduleID]struct{}
	wg       sync.WaitGroup
	errs     chan error
}

// New builds a scheduler over the store for the given operating mode.
func New(
	st *store.Store,
	mode domain.OperatingMode,
	clock func() time.Time,
	kinds map[domain.ScheduleKind]Registration,
) (*Scheduler, error) {
	if st == nil || clock == nil {
		return nil, errors.New("scheduler: nil store or clock")
	}
	if len(kinds) == 0 {
		return nil, errors.New("scheduler: no registered kinds")
	}
	registered := make(map[domain.ScheduleKind]Registration, len(kinds))
	for kind, reg := range kinds {
		if reg.Handle == nil {
			return nil, fmt.Errorf("scheduler: kind %s has no handler", kind)
		}
		if kind == domain.ScheduleInstallationPoll && reg.SubjectLive == nil {
			return nil, fmt.Errorf(
				"scheduler: kind %s requires an activation-state check", kind)
		}
		registered[kind] = reg
	}
	return &Scheduler{
		store: st, mode: mode, clock: clock, kinds: registered,
		inFlight: map[domain.ScheduleID]struct{}{},
		errs:     make(chan error, 1),
	}, nil
}

// RegisteredKinds enumerates the kinds this scheduler drives, sorted, for
// composition coverage tests: the union is closed, so a composition that
// owns every kind can prove it against domain.AllScheduleKinds.
func (s *Scheduler) RegisteredKinds() []domain.ScheduleKind {
	kinds := make([]domain.ScheduleKind, 0, len(s.kinds))
	for kind := range s.kinds {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// Arm converges the stored schedule onto desired within its own
// transaction; see Converge.
func (s *Scheduler) Arm(ctx context.Context, desired domain.Schedule, firstFireAt time.Time) error {
	return s.store.Write(ctx, func(tx *store.WriteTx) error {
		return Converge(ctx, tx, desired, firstFireAt)
	})
}

// Converge converges the stored schedule onto desired inside the caller's
// transaction (so an arming can commit atomically with the state that
// warrants it, e.g. an attention item's creation): absent creates it, an
// armed row with the same shape is kept (its clock preserved across
// restarts), and a terminal row or a shape change (a reconfigured interval,
// a new subject binding) re-arms under the next generation. firstFireAt
// seeds the recurring clock only when one is created or replaced.
func Converge(ctx context.Context, tx *store.WriteTx, desired domain.Schedule, firstFireAt time.Time) error {
	if err := desired.Validate(); err != nil {
		return fmt.Errorf("arm schedule: %w", err)
	}
	if desired.Status != domain.ScheduleArmed {
		return fmt.Errorf("arm schedule %s: status %s", desired.ID, desired.Status)
	}
	existing, err := tx.GetSchedule(ctx, desired.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := tx.PutSchedule(ctx, desired); err != nil {
			return err
		}
		return seedTimer(ctx, tx, desired, firstFireAt)
	case err != nil:
		return err
	}
	if existing.Status == domain.ScheduleArmed && sameShape(existing, desired) {
		// Keep the live generation and its clock; re-seed only a missing
		// timer (a pre-crash row always committed with one, but converging
		// here costs nothing and fails safe).
		if !existing.Kind.OneShot() {
			if _, _, ok, err := tx.GetScheduleTimer(ctx, existing.ID); err != nil {
				return err
			} else if !ok {
				return tx.SetScheduleTimer(ctx, existing.ID, existing.Generation, firstFireAt)
			}
		}
		return nil
	}
	reArmed, err := existing.ReArmed(desired.Subject, desired.FireAt, desired.CreatedAt)
	if err != nil {
		return err
	}
	reArmed.IntervalSeconds = desired.IntervalSeconds
	reArmed.ExpiresAt = desired.ExpiresAt
	reArmed.BaseWatch = desired.BaseWatch
	if err := reArmed.Validate(); err != nil {
		return err
	}
	if err := tx.PutSchedule(ctx, reArmed); err != nil {
		return err
	}
	if err := settlePending(ctx, &tx.InternalTx, reArmed.ID,
		domain.OutcomeStaleGeneration, desired.CreatedAt); err != nil {
		return err
	}
	return seedTimer(ctx, tx, reArmed, firstFireAt)
}

func seedTimer(
	ctx context.Context, tx *store.WriteTx, schedule domain.Schedule, firstFireAt time.Time,
) error {
	if schedule.Kind.OneShot() {
		return tx.DeleteScheduleTimer(ctx, schedule.ID)
	}
	return tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, firstFireAt)
}

// sameShape reports whether the stored schedule already encodes the desired
// binding and cadence; CreatedAt and Generation are deliberately excluded,
// since a restart re-arming an unchanged schedule must not churn either.
func sameShape(a, b domain.Schedule) bool {
	normalize := func(s domain.Schedule) domain.Schedule {
		s.Generation = 1
		s.CreatedAt = time.Time{}
		s.Status = domain.ScheduleArmed
		s.Resolution = nil
		return s
	}
	aj, errA := json.Marshal(normalize(a))
	bj, errB := json.Marshal(normalize(b))
	if errA != nil || errB != nil {
		return false
	}
	return string(aj) == string(bj)
}

// Run drives passes immediately and then on interval until ctx is
// canceled, waiting out in-flight handlers on shutdown. A correctness
// error — from a pass, a handler, or a consumption — stops the loop
// instead of being hidden by retries, mirroring the engine's reconcile
// loops (and, for the janitor kind, preserving "a stopped janitor stops
// the daemon").
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("scheduler: interval %s must be positive", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer s.wg.Wait()
	for {
		if err := s.pass(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.errs:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-ticker.C:
		}
	}
}

// pass performs one scheduling pass: redeliver pending occurrences, then
// fire due schedules. Exported to the composition only through Run; tests
// drive it via RunOnce.
func (s *Scheduler) pass(ctx context.Context) error {
	now := s.clock().UTC()
	type pendingState struct {
		occurrence domain.ScheduleOccurrence
		schedule   domain.Schedule
		found      bool
	}
	var (
		pending []pendingState
		due     []store.DueSchedule
	)
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		occs, err := tx.ListPendingScheduleOccurrences(ctx)
		if err != nil {
			return err
		}
		for _, occ := range occs {
			state := pendingState{occurrence: occ}
			schedule, err := tx.GetSchedule(ctx, occ.ScheduleID)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return err
			default:
				state.schedule = schedule
				state.found = true
			}
			pending = append(pending, state)
		}
		due, err = tx.ListDueSchedules(ctx, now)
		return err
	}); err != nil {
		return fmt.Errorf("scheduler pass: %w", err)
	}

	handled := map[domain.ScheduleID]bool{}
	for _, p := range pending {
		handled[p.occurrence.ScheduleID] = true
		if s.isInFlight(p.occurrence.ScheduleID) {
			continue
		}
		if !p.found {
			// The aggregate is gone but its fire survived: record the proof
			// rather than redelivering against nothing.
			if err := s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.ConsumeScheduleOccurrence(ctx,
					p.occurrence.ScheduleID, p.occurrence.Generation,
					p.occurrence.NominalFireAt, domain.OutcomeConditionNoLongerApplies, now)
				return err
			}); err != nil {
				return fmt.Errorf("scheduler pass: %w", err)
			}
			continue
		}
		if _, ok := s.kinds[p.schedule.Kind]; !ok {
			continue
		}
		if p.schedule.Generation != p.occurrence.Generation || p.schedule.Status != domain.ScheduleArmed {
			if err := s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.ConsumeScheduleOccurrence(ctx,
					p.occurrence.ScheduleID, p.occurrence.Generation,
					p.occurrence.NominalFireAt, domain.OutcomeStaleGeneration, now)
				return err
			}); err != nil {
				return fmt.Errorf("scheduler pass: %w", err)
			}
			continue
		}
		if err := s.fire(ctx, p.schedule, &p.occurrence, now); err != nil {
			return fmt.Errorf("scheduler pass: %w", err)
		}
	}

	for _, d := range due {
		if handled[d.Schedule.ID] || s.isInFlight(d.Schedule.ID) {
			continue
		}
		if _, ok := s.kinds[d.Schedule.Kind]; !ok {
			continue
		}
		if err := s.fire(ctx, d.Schedule, nil, now); err != nil {
			return fmt.Errorf("scheduler pass: %w", err)
		}
	}
	return nil
}

// RunOnce performs one synchronous pass and waits for every fire it
// dispatched to consume: the deterministic form Run loops over, used by
// compositions that drive the scheduler to a condition (the onboarding
// CLI) and by tests.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if err := s.pass(ctx); err != nil {
		return err
	}
	s.wg.Wait()
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

// fire validates one due schedule (§5.16 fire-time validation), converges
// its pending occurrence onto the latest nominal fire with a recorded gap,
// and dispatches the handler. redelivered is the already-pending occurrence
// when this fire is a redelivery, nil when the occurrence must be created.
func (s *Scheduler) fire(
	ctx context.Context,
	schedule domain.Schedule,
	redelivered *domain.ScheduleOccurrence,
	now time.Time,
) error {
	reg := s.kinds[schedule.Kind]

	// Operating-mode eligibility: an ineligible kind simply does not fire;
	// the schedule stays armed and a later eligible pass coalesces the
	// missed fires with a recorded gap.
	if !schedule.Kind.EligibleIn(s.mode) {
		return nil
	}

	// Authority is reconstructed from durable state on every fire, before any
	// terminal outcome can consume the schedule. Workload kinds must lead
	// through their item to the exact run and authenticated resolved policy it
	// is bound to; installation intent and permanent trusted-config jobs use
	// their own closed authority classes instead of a per-run policy.
	if err := s.validateFireAuthority(ctx, schedule); err != nil {
		return err
	}

	// Expiry precedes event construction: an expired schedule terminates
	// durably, and any pending fire is recorded as no longer applicable.
	if schedule.ExpiresAt != nil && !now.Before(*schedule.ExpiresAt) {
		expired, err := schedule.Concluded(domain.ScheduleExpired, domain.ResolutionIntentExpired, now)
		if err != nil {
			return err
		}
		return s.conclude(ctx, expired, now)
	}

	// Subject existence and project binding. A dead subject records its
	// proof (§5.16: never silently discarded); a binding that contradicts
	// the aggregate is corruption and fails loud.
	live, err := s.subjectLive(ctx, reg, schedule)
	if err != nil {
		return err
	}
	if !live {
		resolved, err := schedule.Concluded(domain.ScheduleResolved, domain.ResolutionSubjectConcluded, now)
		if err != nil {
			return err
		}
		return s.conclude(ctx, resolved, now)
	}

	occ, deliverable, err := s.convergeOccurrence(ctx, schedule, redelivered, now)
	if err != nil || !deliverable {
		return err
	}

	ev, err := domain.NewScheduleEvent(schedule, occ, now)
	if err != nil {
		return err
	}
	s.markInFlight(schedule.ID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.clearInFlight(schedule.ID)
		consumption, err := reg.Handle(ctx, ev, schedule)
		if err != nil {
			s.report(fmt.Errorf("schedule %s handler: %w", schedule.ID, err))
			return
		}
		if err := s.consume(ctx, schedule, occ, consumption); err != nil {
			s.report(fmt.Errorf("schedule %s consumption: %w", schedule.ID, err))
		}
	}()
	return nil
}

// validateFireAuthority proves the kind-specific authority immediately
// before event construction. The switch is intentionally exhaustive: adding
// a schedule kind must state whether its authority is a run-bound resolved
// policy, the installation authority document, or daemon-owned trusted
// configuration.
func (s *Scheduler) validateFireAuthority(ctx context.Context, schedule domain.Schedule) error {
	switch schedule.Kind {
	case domain.SchedulePRChecksDeadline,
		domain.ScheduleReviewWaitThreshold,
		domain.ScheduleBaseAdvanceWatch:
		return s.validateResolvedPolicy(ctx, schedule)
	case domain.ScheduleInstallationPoll:
		// subjectLive will revalidate the pending envelope's registration,
		// active epoch, and durable intent revision before event construction.
		// That authority document, not a workflow run, owns installation intent.
		return nil
	case domain.ScheduleDoctor, domain.ScheduleJanitor:
		// These permanent jobs are armed only from daemon-owned trusted
		// configuration and deliberately have no per-run authority.
		return nil
	}
	return fmt.Errorf("schedule %s: kind %q has no authority class", schedule.ID, schedule.Kind)
}

// validateResolvedPolicy follows an attention-item schedule's immutable
// subject binding to its run, then authenticates the stored policy content
// and the run's digest binding. Every mismatch is corruption, not a dead
// subject: fail loud without constructing or consuming an event.
func (s *Scheduler) validateResolvedPolicy(ctx context.Context, schedule domain.Schedule) error {
	return s.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, *schedule.Subject.ItemID)
		if err != nil {
			return fmt.Errorf("schedule %s resolved-policy item: %w", schedule.ID, err)
		}
		if item.ProjectID != schedule.ProjectID {
			return fmt.Errorf("schedule %s project %s binds item %s of project %s",
				schedule.ID, schedule.ProjectID, item.ID, item.ProjectID)
		}
		if item.Subject.Type != domain.SubjectRun || item.Subject.RunID == nil ||
			item.Subject.ID != domain.SubjectID(*item.Subject.RunID) {
			return fmt.Errorf("schedule %s item %s does not identify one exact run",
				schedule.ID, item.ID)
		}
		if *item.Subject.RunID != *schedule.RunID {
			return fmt.Errorf("schedule %s run %s binds item %s retargeted to run %s",
				schedule.ID, *schedule.RunID, item.ID, *item.Subject.RunID)
		}
		run, err := tx.GetRun(ctx, *item.Subject.RunID)
		if err != nil {
			return fmt.Errorf("schedule %s resolved-policy run %s: %w",
				schedule.ID, *item.Subject.RunID, err)
		}
		if run.ProjectID != schedule.ProjectID {
			return fmt.Errorf("schedule %s project %s binds run %s of project %s",
				schedule.ID, schedule.ProjectID, run.ID, run.ProjectID)
		}
		policy, err := tx.GetResolvedPolicy(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("schedule %s resolved policy for run %s: %w",
				schedule.ID, run.ID, err)
		}
		if policy.RunID != run.ID || policy.Digest != run.PolicyDigest ||
			run.PolicyDigest != *schedule.PolicyDigest {
			return fmt.Errorf(
				"schedule %s run %s policy binding mismatch: run digest %q, policy run %s digest %q",
				schedule.ID, run.ID, run.PolicyDigest, policy.RunID, policy.Digest)
		}
		return nil
	})
}

// convergeOccurrence produces the pending occurrence to deliver: the latest
// nominal fire at or before now, coalescing an older pending occurrence and
// missed recurring fires into one with a recorded gap. deliverable is false
// when the identity was already consumed (a completed fire re-observed
// before its schedule advanced).
func (s *Scheduler) convergeOccurrence(
	ctx context.Context,
	schedule domain.Schedule,
	redelivered *domain.ScheduleOccurrence,
	now time.Time,
) (domain.ScheduleOccurrence, bool, error) {
	nominal, gap, next, live, err := s.latestNominal(ctx, schedule, redelivered, now)
	if err != nil || !live {
		return domain.ScheduleOccurrence{}, false, err
	}
	if redelivered != nil && redelivered.NominalFireAt.Equal(nominal) {
		return *redelivered, true, nil
	}
	occ := domain.ScheduleOccurrence{
		ScheduleID: schedule.ID, Generation: schedule.Generation,
		NominalFireAt: nominal, Status: domain.OccurrencePending,
		Gap: gap, CreatedAt: now,
	}
	deliverable := true
	if err := s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if next != nil {
			// Transactional form of the clock checks above: between that read
			// and this write the timer could have been re-armed (a different
			// generation) or advanced past now by a concurrent scheduler
			// process, and creating the occurrence or advancing the clock
			// here would deliver a future nominal early or overwrite the
			// other generation's cadence.
			timerGeneration, timerNext, ok, err := tx.GetScheduleTimer(ctx, schedule.ID)
			if err != nil {
				return err
			}
			if !ok || timerGeneration != schedule.Generation || timerNext.After(now) {
				deliverable = false
				return nil
			}
		}
		if redelivered != nil {
			// The older pending fire is superseded by the latest nominal
			// occurrence, which carries the folded gap.
			if _, err := tx.ConsumeScheduleOccurrence(ctx,
				redelivered.ScheduleID, redelivered.Generation,
				redelivered.NominalFireAt, domain.OutcomeCoalesced, now); err != nil {
				return err
			}
		}
		created, err := tx.CreatePendingScheduleOccurrence(ctx, occ)
		if err != nil {
			return err
		}
		if !created {
			existing, err := tx.GetScheduleOccurrence(ctx, occ.ScheduleID, occ.Generation, occ.NominalFireAt)
			if err != nil {
				return err
			}
			if existing.Status != domain.OccurrencePending {
				deliverable = false
				return nil
			}
			occ = existing
		}
		if next != nil {
			return tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, *next)
		}
		return nil
	}); err != nil {
		return domain.ScheduleOccurrence{}, false, err
	}
	return occ, deliverable, nil
}

// latestNominal computes the §5.16 coalescing: the latest nominal fire at
// or before now, the recorded gap it carries, and (for recurring kinds) the
// advanced next-nominal clock. live reports whether the clock still belongs
// to this schedule generation: a concurrent re-arm (the onboarding CLI
// replacing an installation intent while the daemon scheduler runs) can
// replace the timer between the due scan and this read, and firing from —
// or later overwriting — the new generation's clock would silently corrupt
// its cadence.
func (s *Scheduler) latestNominal(
	ctx context.Context,
	schedule domain.Schedule,
	redelivered *domain.ScheduleOccurrence,
	now time.Time,
) (nominal time.Time, gap *domain.ScheduleFireGap, next *time.Time, live bool, err error) {
	if schedule.Kind.OneShot() {
		// A one-shot deadline has exactly one nominal instant; lateness is
		// delivery delay, not missed occurrences.
		return *schedule.FireAt, cloneGap(redelivered), nil, true, nil
	}
	interval := time.Duration(*schedule.IntervalSeconds) * time.Second
	var base time.Time
	if redelivered != nil {
		base = redelivered.NominalFireAt
	} else {
		timerGeneration, timerNext, ok, err := s.readTimer(ctx, schedule.ID)
		if err != nil {
			return time.Time{}, nil, nil, false, err
		}
		if !ok {
			return time.Time{}, nil, nil, false, fmt.Errorf(
				"schedule %s: recurring schedule has no timer", schedule.ID)
		}
		if timerGeneration != schedule.Generation || timerNext.After(now) {
			// A different generation's clock, or one another scheduler
			// process already advanced past now (the daemon and a resumed
			// onboarding CLI can drive the same installation poll): either
			// way this scan's fire is stale, not due.
			return time.Time{}, nil, nil, false, nil
		}
		base = timerNext
	}
	missed := int64(0)
	if now.Sub(base) >= interval {
		missed = int64(now.Sub(base) / interval)
	}
	nominal = base.Add(time.Duration(missed) * interval)
	gap = cloneGap(redelivered)
	if missed > 0 {
		earliest := base
		if gap != nil {
			earliest = gap.EarliestMissedAt
			missed += gap.MissedOccurrences
		}
		gap = &domain.ScheduleFireGap{MissedOccurrences: missed, EarliestMissedAt: earliest}
	}
	advanced := nominal.Add(interval)
	return nominal, gap, &advanced, true, nil
}

func cloneGap(occ *domain.ScheduleOccurrence) *domain.ScheduleFireGap {
	if occ == nil || occ.Gap == nil {
		return nil
	}
	gap := *occ.Gap
	return &gap
}

func (s *Scheduler) readTimer(
	ctx context.Context, id domain.ScheduleID,
) (generation int64, next time.Time, ok bool, err error) {
	err = s.store.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		generation, next, ok, readErr = tx.GetScheduleTimer(ctx, id)
		return readErr
	})
	return generation, next, ok, err
}

// consume commits a handler's outcome atomically with the occurrence's
// consumed mark (§5.16). A one-shot fire must terminate or re-arm its
// schedule — "one-shot deadlines always terminate fired-and-handled or
// explicitly resolved" is enforced here, not trusted to handlers.
func (s *Scheduler) consume(
	ctx context.Context,
	schedule domain.Schedule,
	occ domain.ScheduleOccurrence,
	c Consumption,
) error {
	if schedule.Kind.OneShot() {
		if c.Schedule == nil || (!c.Schedule.Status.Terminal() && c.Schedule.Generation == schedule.Generation) {
			return errors.New("one-shot consumption must conclude or re-arm its schedule")
		}
	}
	now := s.clock().UTC()
	apply := func(tx *store.WriteTx) error {
		// The aggregate is re-read before the handler's snapshot is written:
		// a re-arm or conclusion that serialized between the handler's
		// return and this transaction makes the fire stale, and writing the
		// old generation's outcome would be rejected as a stale transition
		// and needlessly stop the scheduler. (A committed re-arm or
		// conclusion also settles the pending occurrence in its own
		// transaction, so these settles are usually no-ops kept for the
		// window where the occurrence still stands.)
		current, err := tx.GetSchedule(ctx, schedule.ID)
		if errors.Is(err, store.ErrNotFound) {
			_, err := tx.ConsumeScheduleOccurrence(ctx,
				occ.ScheduleID, occ.Generation, occ.NominalFireAt,
				domain.OutcomeConditionNoLongerApplies, now)
			return err
		}
		if err != nil {
			return err
		}
		if current.Generation != schedule.Generation || current.Status != domain.ScheduleArmed {
			_, err := tx.ConsumeScheduleOccurrence(ctx,
				occ.ScheduleID, occ.Generation, occ.NominalFireAt,
				domain.OutcomeStaleGeneration, now)
			return err
		}
		consumed, err := tx.ConsumeScheduleOccurrence(ctx,
			occ.ScheduleID, occ.Generation, occ.NominalFireAt, c.Outcome, now)
		if err != nil {
			return err
		}
		if !consumed {
			// Raced or replayed: the occurrence already settled; applying the
			// outcome again would double-commit it.
			return nil
		}
		if c.Schedule != nil {
			if err := tx.PutSchedule(ctx, *c.Schedule); err != nil {
				return err
			}
			switch {
			case c.Schedule.Status.Terminal():
				if err := tx.DeleteScheduleTimer(ctx, c.Schedule.ID); err != nil {
					return err
				}
				if err := settlePending(ctx, &tx.InternalTx, c.Schedule.ID,
					domain.OutcomeConditionNoLongerApplies, now); err != nil {
					return err
				}
			case c.Schedule.Generation != schedule.Generation:
				if err := settlePending(ctx, &tx.InternalTx, c.Schedule.ID,
					domain.OutcomeStaleGeneration, now); err != nil {
					return err
				}
				if !c.Schedule.Kind.OneShot() {
					interval := time.Duration(*c.Schedule.IntervalSeconds) * time.Second
					if err := tx.SetScheduleTimer(ctx, c.Schedule.ID,
						c.Schedule.Generation, c.Schedule.CreatedAt.Add(interval)); err != nil {
						return err
					}
				}
			}
		}
		if c.Commit != nil {
			return c.Commit(ctx, tx)
		}
		return nil
	}
	if c.Schedule == nil && c.Commit == nil {
		return s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.ConsumeScheduleOccurrence(ctx,
				occ.ScheduleID, occ.Generation, occ.NominalFireAt, c.Outcome, now)
			return err
		})
	}
	if err := s.store.Write(ctx, apply); err != nil {
		if errors.Is(err, ErrStaleConsumption) {
			// The transaction rolled back whole: the occurrence is still
			// pending and the next pass revalidates it.
			return nil
		}
		return err
	}
	return nil
}

// conclude terminates a schedule outside a handler (expiry, dead subject):
// the aggregate change, its timer removal, and the recorded proof on any
// pending occurrence commit together. The aggregate is re-read inside the
// transaction: a concurrent re-arm (or conclusion) between the due scan and
// this write means the scanned snapshot was a stale fire, and the current
// generation owns its own lifecycle — attempting the old transition would
// be rejected as stale and needlessly stop the scheduler.
func (s *Scheduler) conclude(ctx context.Context, concluded domain.Schedule, now time.Time) error {
	return s.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetSchedule(ctx, concluded.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Generation != concluded.Generation || current.Status != domain.ScheduleArmed {
			return nil
		}
		if err := tx.PutSchedule(ctx, concluded); err != nil {
			return err
		}
		if err := tx.DeleteScheduleTimer(ctx, concluded.ID); err != nil {
			return err
		}
		return settlePending(ctx, &tx.InternalTx, concluded.ID,
			domain.OutcomeConditionNoLongerApplies, now)
	})
}

// settlePending settles every pending occurrence of one schedule with the
// given proof outcome, inside the caller's transaction.
func settlePending(
	ctx context.Context, tx *store.InternalTx, id domain.ScheduleID,
	outcome domain.ScheduleOccurrenceOutcome, now time.Time,
) error {
	pending, err := tx.ListPendingScheduleOccurrences(ctx)
	if err != nil {
		return err
	}
	for _, occ := range pending {
		if occ.ScheduleID != id {
			continue
		}
		if _, err := tx.ConsumeScheduleOccurrence(ctx,
			occ.ScheduleID, occ.Generation, occ.NominalFireAt, outcome, now.UTC()); err != nil {
			return err
		}
	}
	return nil
}

// subjectLive resolves the fire-time subject check: the registration's own
// check when supplied, the built-in per-subject-type checks otherwise. A
// project binding that contradicts the subject is corruption, not a dead
// subject, and fails loud.
func (s *Scheduler) subjectLive(
	ctx context.Context, reg Registration, schedule domain.Schedule,
) (bool, error) {
	if reg.SubjectLive != nil {
		return reg.SubjectLive(ctx, schedule)
	}
	switch schedule.Subject.Type {
	case domain.ScheduleSubjectTrustedConfig:
		return true, nil
	case domain.ScheduleSubjectAttentionItem:
		var item domain.AttentionItem
		err := s.store.Read(ctx, func(tx *store.ReadTx) error {
			var readErr error
			item, readErr = tx.GetAttentionItem(ctx, *schedule.Subject.ItemID)
			return readErr
		})
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if item.ProjectID != schedule.ProjectID {
			return false, fmt.Errorf("schedule %s project %s binds item %s of project %s",
				schedule.ID, schedule.ProjectID, item.ID, item.ProjectID)
		}
		return item.Status == domain.StatusOpen, nil
	case domain.ScheduleSubjectInstallationIntent:
		return false, fmt.Errorf(
			"schedule %s: installation_intent subject requires a registered activation check",
			schedule.ID)
	}
	return false, fmt.Errorf("schedule %s: subject type %q", schedule.ID, schedule.Subject.Type)
}

func (s *Scheduler) isInFlight(id domain.ScheduleID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inFlight[id]
	return ok
}

func (s *Scheduler) markInFlight(id domain.ScheduleID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[id] = struct{}{}
}

func (s *Scheduler) clearInFlight(id domain.ScheduleID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, id)
}

func (s *Scheduler) report(err error) {
	select {
	case s.errs <- err:
	default:
	}
}
