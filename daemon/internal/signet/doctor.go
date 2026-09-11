package signet

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// requestDoctor advances the existing diagnostic job in the accepting command
// transaction. The scheduler owns execution, redelivery and health convergence;
// a replayed command never reaches this method and cannot request another pass.
func (s *Service) requestDoctor(ctx context.Context, tx *store.WriteTx) error {
	if s.doctorSchedule == "" {
		return fmt.Errorf("%w: schedule is not configured", ErrDoctorUnavailable)
	}
	if s.doctorAvailable == nil || !s.doctorAvailable() {
		return fmt.Errorf("%w: start or restart the diagnostic service", ErrDoctorUnavailable)
	}
	schedule, err := tx.GetSchedule(ctx, s.doctorSchedule)
	if err != nil {
		return fmt.Errorf("read doctor schedule: %w", err)
	}
	if schedule.Kind != domain.ScheduleDoctor || schedule.Status != domain.ScheduleArmed {
		return fmt.Errorf("%w: configured schedule is not armed", ErrDoctorUnavailable)
	}
	generation, next, ok, err := tx.GetScheduleTimer(ctx, schedule.ID)
	if err != nil {
		return err
	}
	if !ok || generation != schedule.Generation {
		return fmt.Errorf("%w: schedule has no current timer", ErrDoctorUnavailable)
	}
	now := s.now().UTC()
	if !next.After(now) {
		return nil // A due pass already covers this request; never postpone it.
	}
	return tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, now)
}
