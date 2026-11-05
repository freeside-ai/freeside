package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Durable-scheduler composition for the production (claude) daemon: the
// §5.16 permanent trusted-config jobs. The doctor and janitor keep their
// §10 obligations — including in attended_dev — with only their cadence
// migrated off plain tickers; each keeps its synchronous startup pass
// (main.go's initial doctor run, the janitor's coverage priming in
// composeClaudeDriver) as a direct call. A handler failure stops the
// scheduler loop, which the daemon consumes as a durable in-process stop.

const (
	schedulerSystemProjectID = domain.ProjectID("project-system")
	doctorScheduleID         = domain.ScheduleID("schedule-doctor")
	janitorScheduleID        = domain.ScheduleID("schedule-janitor")
	imageUpkeepScheduleID    = domain.ScheduleID("schedule-image-upkeep")
)

// pullObserver and issueObserver read one bound resource's state. The
// production composition observes through the publish reconciler's
// conditional reads; the fake lane's world holds PRs that never merge and
// issues that never close.
type (
	pullObserver  func(ctx context.Context, repo string, number int) (publish.PullObservation, error)
	issueObserver func(ctx context.Context, repo string, number int) (publish.IssueObservation, error)
)

// labelIssueObserver reads the open issues carrying one initiator label,
// conditionally through the publish reconciler (ReconcileLabelIssues). The
// label-intake loop consumes it to allocate occurrences; the fake lane wires no
// label observer.
type labelIssueObserver func(ctx context.Context, repo, label string) (publish.LabelIssuesObservation, error)

// nativeReviewObserver reads one PR's native (forge-hosted) review activity —
// its submitted reviews, inline review comments, and description reactions —
// as best-effort extra evidence (plan §5.16, §7). The production composition
// observes through the publish reconciler's conditional reads; the fake lane
// wires no native reviewer.
type nativeReviewObserver func(ctx context.Context, repo string, number int) (publish.PullReviewObservation, error)

// deadlineRegistration wires a one-shot publication deadline
// (pr_checks_deadline, review_wait_threshold). The handler first rechecks
// the event's expected subject version (§5.16): a stale expectation — the
// base-advance watch's fact write, say, bumped the item — re-arms under a
// new generation with the corrected binding and the same nominal deadline,
// so the elapsed wall time is never postponed and the deadline fires again
// on the next pass against the current binding. A matching, still-open item
// terminates fired-and-handled with deadline_elapsed on the synced
// aggregate. Richer consumers (surfacing the elapsed deadline as attention,
// binding to observed check runs) arrive with the §7 review stage (#427).
func deadlineRegistration(st *store.Store) scheduler.Registration {
	return scheduler.Registration{Handle: func(
		ctx context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
	) (scheduler.Consumption, error) {
		current, consumption, err := recheckItemSubject(ctx, st, ev, sc)
		if err != nil || consumption != nil {
			if consumption != nil {
				return *consumption, err
			}
			return scheduler.Consumption{}, err
		}
		if current.Status != domain.StatusOpen {
			// Concluded between fire-time validation and consumption: record
			// the proof rather than a deadline that no longer applies.
			resolved, err := sc.Concluded(
				domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			return scheduler.Consumption{
				Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
			}, nil
		}
		fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
		if err != nil {
			return scheduler.Consumption{}, err
		}
		return scheduler.Consumption{
			Outcome: domain.OutcomeHandled, Schedule: &fired,
			Commit: func(ctx context.Context, tx *store.WriteTx) error {
				// The decisive check re-runs inside the consuming transaction:
				// an operator conclusion that serialized after the handler's
				// read abandons this consumption, and the next pass records
				// the subject's conclusion instead of a stale
				// deadline_elapsed.
				item, err := tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
				if errors.Is(err, store.ErrNotFound) {
					return scheduler.ErrStaleConsumption
				}
				if err != nil {
					return err
				}
				if item.Status != domain.StatusOpen || item.ItemVersion != *ev.Subject.ItemVersion {
					return scheduler.ErrStaleConsumption
				}
				return nil
			},
		}, nil
	}}
}

// recheckItemSubject is the consuming handler's §5.16 expectation recheck
// for attention-item subjects, shared by both deadline kinds:
// a vanished item resolves with recorded proof, and a moved item version
// re-arms with the corrected binding (the deadline keeps its nominal fire
// instant, so the re-armed generation is due immediately). A nil
// consumption with a nil error means the expectation held and the caller
// proceeds with the current item.
func recheckItemSubject(
	ctx context.Context, st *store.Store, ev domain.ScheduleEvent, sc domain.Schedule,
) (domain.AttentionItem, *scheduler.Consumption, error) {
	var current domain.AttentionItem
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		current, readErr = tx.GetAttentionItem(ctx, *sc.Subject.ItemID)
		return readErr
	})
	if errors.Is(err, store.ErrNotFound) {
		resolved, err := sc.Concluded(
			domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
		if err != nil {
			return domain.AttentionItem{}, nil, err
		}
		return domain.AttentionItem{}, &scheduler.Consumption{
			Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
		}, nil
	}
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	if current.ItemVersion != *ev.Subject.ItemVersion {
		version := current.ItemVersion
		subject := sc.Subject
		subject.ItemVersion = &version
		reArmed, err := sc.ReArmed(subject, sc.FireAt, ev.FiredAt)
		if err != nil {
			return domain.AttentionItem{}, nil, err
		}
		return domain.AttentionItem{}, &scheduler.Consumption{
			Outcome: domain.OutcomeReArmed, Schedule: &reArmed,
		}, nil
	}
	return current, nil, nil
}

// publicationWatchRegistrations is the shared registration set for the two
// §5.16 publication deadline kinds, identical in both daemon compositions.
// Work-unit completion after a merge is the active-resource reconciler's
// (§5.18), not a schedule's.
func publicationWatchRegistrations(st *store.Store) map[domain.ScheduleKind]scheduler.Registration {
	return map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline:    deadlineRegistration(st),
		domain.ScheduleReviewWaitThreshold: deadlineRegistration(st),
	}
}

// newFakeScheduler runs the publication watch kinds for the walking-skeleton
// composition, keeping fake-lane parity with the production wiring.
func newFakeScheduler(st *store.Store) (*scheduler.Scheduler, error) {
	return scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return time.Now().UTC() },
		publicationWatchRegistrations(st))
}

func newClaudeScheduler(
	st *store.Store,
	cfg config,
	wiring *claudeComposition,
	runDoctor func(context.Context) error,
	collectUsage func(context.Context) error,
	imageUpkeep *engine.ImageUpkeep,
) (*scheduler.Scheduler, error) {
	now := func() time.Time { return time.Now().UTC() }
	failures := newScheduledFailureTracker(now, cfg.Logger)
	upkeepLogger := cfg.Logger
	if upkeepLogger == nil {
		upkeepLogger = slog.New(slog.DiscardHandler)
	}
	kinds := map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleDoctor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			err := runScheduledDoctorPass(ctx, wiring.runConformance, runDoctor)
			if err != nil {
				err = fmt.Errorf("scheduled doctor pass: %w", err)
			}
			// Each pass starts usage collection without waiting for it (plan
			// §10). A collection failure shows only as its pool's refresh
			// status, so it stays out of the consumption: a failed doctor
			// pass stops the daemon.
			if collectErr := collectUsage(ctx); collectErr != nil && ctx.Err() == nil {
				upkeepLogger.Warn("scheduled usage collection did not start", "error", collectErr)
			}
			return failures.consumption(domain.ScheduleDoctor, err)
		}},
		domain.ScheduleJanitor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			err := wiring.janitor.RunScheduledPass(ctx)
			if err == nil {
				err = janitorRegistrationFailures(wiring.janitor.RegistrationFaults())
			}
			if err != nil {
				err = fmt.Errorf("installation janitor: %w", err)
			}
			return failures.consumption(domain.ScheduleJanitor, err)
		}},
		domain.ScheduleImageUpkeep: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			// A failed pass decided nothing and changed nothing, and work
			// still runs in the configured image, so the failure is logged
			// and the next fire retries; it does not stop the daemon as a
			// doctor or janitor failure does. What a person must fix is
			// recorded as a host fault, which files an item.
			if err := imageUpkeep.RunScheduled(ctx); err != nil {
				if ctx.Err() != nil {
					return scheduler.Consumption{}, err
				}
				upkeepLogger.Warn("scheduled image upkeep pass failed", "error", err)
				return scheduler.Consumption{Outcome: domain.OutcomeObserveFailed}, nil
			}
			return failures.consumption(domain.ScheduleImageUpkeep, nil)
		}},
	}
	// The installation-poll kind registers with the daemon too: a pending
	// intent recorded by the onboarding CLI keeps its durable observation
	// (and its expiry gets recorded) even when the operator never resumes.
	kinds[domain.ScheduleInstallationPoll] = installPollRegistration(wiring.authority, wiring.janitor)
	for kind, reg := range publicationWatchRegistrations(st) {
		kinds[kind] = reg
	}
	return scheduler.New(st, cfg.Claude.OperatingMode, now, kinds)
}

// armTrustedConfigJobs converges the doctor and janitor schedules onto the
// current configuration. An unchanged schedule keeps its durable clock, so
// a restart preserves cadence (a missed fire coalesces with a recorded gap
// rather than resetting); a reconfigured interval re-arms under the next
// generation.
func armTrustedConfigJobs(ctx context.Context, sched *scheduler.Scheduler, cfg config) error {
	now := time.Now().UTC()
	for _, job := range []struct {
		id       domain.ScheduleID
		kind     domain.ScheduleKind
		interval time.Duration
	}{
		{doctorScheduleID, domain.ScheduleDoctor, cfg.DoctorInterval},
		{janitorScheduleID, domain.ScheduleJanitor, defaultJanitorInterval},
		// Upkeep's start pass runs beside startup rather than before it, and
		// its scheduled pass is the retry for a host fault, at the doctor's
		// cadence.
		{imageUpkeepScheduleID, domain.ScheduleImageUpkeep, cfg.DoctorInterval},
	} {
		seconds := int64(job.interval / time.Second)
		schedule, err := domain.NewSchedule(domain.ScheduleInput{
			ID: job.id, ProjectID: schedulerSystemProjectID, Kind: job.kind,
			Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
			CreatedAt: now, IntervalSeconds: &seconds,
		})
		if err != nil {
			return fmt.Errorf("arm %s: %w", job.id, err)
		}
		// The startup obligation already ran synchronously, so a fresh
		// schedule's first fire is one interval out; an existing schedule
		// keeps its own clock.
		if err := sched.Arm(ctx, schedule, now.Add(job.interval)); err != nil {
			return fmt.Errorf("arm %s: %w", job.id, err)
		}
	}
	return nil
}
