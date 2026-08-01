package main

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The §5.16 installation-poll kind: the durable form of the onboarding
// pending-install-or-expansion poll (§10). One derivation path serves both
// compositions — the onboarding CLI, which drives the scheduler to a
// resolution, and the daemon, which registers the kind so a still-pending
// intent keeps its durable observation and its expiry gets recorded even if
// the operator never resumes. The handler only observes and resolves; it
// never promotes: promotion extends trust, and firing never extends or
// preserves authority (§5.16), so it stays an operator-driven onboarding
// act (operations.PromoteInstallation).

// installPollInterval is the durable poll cadence. The pre-1B poll ticked
// every 100ms against in-memory janitor coverage; a durable occurrence per
// tick makes that cadence absurd, and GitHub's native install flow takes
// tens of seconds, so two seconds keeps resume latency imperceptible.
const installPollInterval = 2 * time.Second

// pendingReadySource is the janitor's onboarding transition signal
// (publish.InstallationJanitor and the daemon's janitorSession both
// provide it).
type pendingReadySource interface {
	PendingReady(publish.PendingInstallationEnvelope) (int64, bool)
}

func installPollScheduleID(registrationID int64) domain.ScheduleID {
	return domain.ScheduleID(fmt.Sprintf("schedule-installation_poll-%d", registrationID))
}

// installPollRegistration wires the installation-poll kind. SubjectLive is
// the §5.9 activation-state check: the schedule stays live only while the
// authority document holds the exact pending envelope (epoch and durable
// intent revision) the schedule was armed against; a promoted or superseded
// envelope resolves as subject_concluded at the next fire.
func installPollRegistration(
	authority publish.InstallationAuthoritySource,
	ready pendingReadySource,
) scheduler.Registration {
	pendingEnvelope := func(ctx context.Context, sc domain.Schedule) (*publish.PendingInstallationEnvelope, error) {
		snapshot, err := authority.InstallationAuthority(ctx, *sc.Subject.RegistrationID)
		if err != nil {
			return nil, err
		}
		p := snapshot.Pending
		if p == nil || p.ActiveEpoch != *sc.Subject.ActiveEpoch ||
			p.DurableIntentRevision != *sc.Subject.DurableIntentRevision {
			return nil, nil
		}
		return p, nil
	}
	return scheduler.Registration{
		SubjectLive: func(ctx context.Context, sc domain.Schedule) (bool, error) {
			p, err := pendingEnvelope(ctx, sc)
			return p != nil, err
		},
		Handle: func(ctx context.Context, ev domain.ScheduleEvent, sc domain.Schedule) (scheduler.Consumption, error) {
			p, err := pendingEnvelope(ctx, sc)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			if p == nil {
				// The envelope moved between fire-time validation and
				// consumption: record the proof rather than polling nothing.
				resolved, err := sc.Concluded(
					domain.ScheduleResolved, domain.ResolutionSubjectConcluded, ev.FiredAt)
				if err != nil {
					return scheduler.Consumption{}, err
				}
				return scheduler.Consumption{
					Outcome: domain.OutcomeConditionNoLongerApplies, Schedule: &resolved,
				}, nil
			}
			if _, isReady := ready.PendingReady(*p); isReady {
				resolved, err := sc.Concluded(
					domain.ScheduleResolved, domain.ResolutionConditionSatisfied, ev.FiredAt)
				if err != nil {
					return scheduler.Consumption{}, err
				}
				return scheduler.Consumption{Outcome: domain.OutcomeHandled, Schedule: &resolved}, nil
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		},
	}
}

func newInstallPollScheduler(
	st *store.Store,
	authority publish.InstallationAuthoritySource,
	ready pendingReadySource,
) (*scheduler.Scheduler, error) {
	return scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return time.Now().UTC() },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleInstallationPoll: installPollRegistration(authority, ready),
		})
}

// armInstallationPoll converges the durable poll schedule onto the current
// pending envelope: armed at intent recording, re-armed when the envelope's
// epoch or revision moved, expiring exactly when the envelope does.
func armInstallationPoll(
	ctx context.Context,
	sched *scheduler.Scheduler,
	registrationID, activeEpoch, durableIntentRevision int64,
	expiresAt time.Time,
) error {
	interval := int64(installPollInterval / time.Second)
	expiry := expiresAt.UTC()
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: installPollScheduleID(registrationID), ProjectID: schedulerSystemProjectID,
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: &registrationID, ActiveEpoch: &activeEpoch,
			DurableIntentRevision: &durableIntentRevision,
		},
		CreatedAt: time.Now().UTC(), IntervalSeconds: &interval, ExpiresAt: &expiry,
	})
	if err != nil {
		return fmt.Errorf("arm installation poll: %w", err)
	}
	if err := sched.Arm(ctx, schedule, time.Now().UTC()); err != nil {
		return fmt.Errorf("arm installation poll: %w", err)
	}
	return nil
}

// awaitInstallationPoll drives the scheduler until the durable poll
// concludes: resolved condition_satisfied means the selected grant matched
// and onboarding proceeds; expiry is the -install-wait bound, now carried
// by the envelope itself so it holds across sessions.
func awaitInstallationPoll(
	ctx context.Context,
	st *store.Store,
	sched *scheduler.Scheduler,
	id domain.ScheduleID,
) error {
	ticker := time.NewTicker(installPollInterval / 2)
	defer ticker.Stop()
	for {
		if err := sched.RunOnce(ctx); err != nil {
			return err
		}
		var schedule domain.Schedule
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			schedule, err = tx.GetSchedule(ctx, id)
			return err
		}); err != nil {
			return err
		}
		switch schedule.Status {
		case domain.ScheduleArmed:
		case domain.ScheduleResolved:
			if schedule.Resolution.Reason == domain.ResolutionConditionSatisfied {
				return nil
			}
			return fmt.Errorf("pending installation intent disappeared (%s)",
				schedule.Resolution.Reason)
		case domain.ScheduleExpired:
			return fmt.Errorf("wait for selected installation: %w", context.DeadlineExceeded)
		case domain.ScheduleFired:
			return fmt.Errorf("installation poll %s terminated %s", id, schedule.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// noPendingReady arms schedules in compositions that observe no janitor (the
// Begin path records the intent and exits before any poll runs).
type noPendingReady struct{}

func (noPendingReady) PendingReady(publish.PendingInstallationEnvelope) (int64, bool) {
	return 0, false
}
