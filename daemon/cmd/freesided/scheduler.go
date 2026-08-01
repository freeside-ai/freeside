package main

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Durable-scheduler composition for the production (claude) daemon: the
// §5.16 permanent trusted-config jobs. The doctor and janitor keep their
// §10 obligations — including in attended_dev — with only their cadence
// migrated off plain tickers; each keeps its synchronous startup pass
// (main.go's initial doctor run, the janitor's coverage priming in
// composeClaudeDriver) as a direct call, and a handler failure stops the
// scheduler loop, which the daemon treats as fatal.

const (
	schedulerSystemProjectID = domain.ProjectID("project-system")
	doctorScheduleID         = domain.ScheduleID("schedule-doctor")
	janitorScheduleID        = domain.ScheduleID("schedule-janitor")
)

func newClaudeScheduler(
	st *store.Store,
	cfg config,
	wiring *claudeComposition,
	runDoctor func(context.Context) error,
) (*scheduler.Scheduler, error) {
	kinds := map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleDoctor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			if err := runScheduledDoctorPass(ctx, wiring.runConformance, runDoctor); err != nil {
				return scheduler.Consumption{}, fmt.Errorf("scheduled doctor pass: %w", err)
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
		domain.ScheduleJanitor: {Handle: func(
			ctx context.Context, _ domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			if err := wiring.janitor.RunScheduledPass(ctx); err != nil {
				return scheduler.Consumption{}, fmt.Errorf("installation janitor: %w", err)
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	}
	// The installation-poll kind registers with the daemon too: a pending
	// intent recorded by the onboarding CLI keeps its durable observation
	// (and its expiry gets recorded) even when the operator never resumes.
	kinds[domain.ScheduleInstallationPoll] = installPollRegistration(wiring.authority, wiring.janitor)
	return scheduler.New(st, cfg.Claude.OperatingMode,
		func() time.Time { return time.Now().UTC() }, kinds)
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
