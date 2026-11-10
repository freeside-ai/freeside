package daemoncompose

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
)

// PendingReadySource is the janitor's onboarding transition signal
// (publish.InstallationJanitor and the daemon's janitorSession both
// provide it).
type PendingReadySource interface {
	PendingReady(publish.PendingInstallationEnvelope) (int64, bool)
}

// InstallPollRegistration wires the installation-poll kind. SubjectLive is
// the §5.9 activation-state check: the schedule stays live only while the
// authority document holds the exact pending envelope (epoch and durable
// intent revision) the schedule was armed against; a promoted or superseded
// envelope resolves as subject_concluded at the next fire.
func InstallPollRegistration(
	authority publish.InstallationAuthoritySource,
	ready PendingReadySource,
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
