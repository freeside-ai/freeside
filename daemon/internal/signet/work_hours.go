package signet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrWorkHoursProjectNotFound refuses a work-hours read limited to a project
// the daemon doesn't have. It is the read's only not-found: any other
// missing record is a broken reference and fails the read.
var ErrWorkHoursProjectNotFound = errors.New("work hours project not found")

// WorkHoursRequest is a work-hours read: the period, and the project to
// limit it to, or nil for every project.
type WorkHoursRequest struct {
	Period    domain.WorkHoursPeriod
	ProjectID *domain.ProjectID
}

// WorkHours divides every stored run's time into classes over the days of
// the request's period (domain.WorkHoursOf), in one read transaction. Its
// as_of is the service clock read inside the transaction after the runs,
// as GetRunTimeline reads it, so no milestone the read sees is later than
// as_of. It writes nothing and decides nothing (issue #3241).
//
// Each run is read the way the timelines read it, so a run's hours agree
// with the clocks its timeline serves. The figures are advisory: a run whose
// record fails the integrity check is skipped, logged, and counted, and the
// rest still serve.
func (s *Service) WorkHours(ctx context.Context, request WorkHoursRequest) (domain.WorkHours, error) {
	var out domain.WorkHours
	var excluded []excludedRun
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		out, excluded, err = readWorkHours(ctx, tx, request, s.now)
		return err
	})
	if err != nil {
		return domain.WorkHours{}, fmt.Errorf("work hours: %w", err)
	}
	// Logged after the read transaction closes, never inside it. Unlike
	// ListRuns, the read doesn't converge the system-health item: that
	// writes, and this read writes nothing.
	s.logExcludedRuns(excluded)
	return out, nil
}

func readWorkHours(
	ctx context.Context, tx *store.ReadTx, request WorkHoursRequest, now func() time.Time,
) (domain.WorkHours, []excludedRun, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return domain.WorkHours{}, nil, err
	}
	if err := validateServerState(state); err != nil {
		return domain.WorkHours{}, nil, err
	}
	if request.ProjectID != nil {
		if _, err := tx.GetProject(ctx, *request.ProjectID); errors.Is(err, store.ErrNotFound) {
			return domain.WorkHours{}, nil, fmt.Errorf("project %q: %w", *request.ProjectID, ErrWorkHoursProjectNotFound)
		} else if err != nil {
			return domain.WorkHours{}, nil, err
		}
	}
	holdsSince, err := tx.HoldsRecordedSince(ctx)
	if err != nil {
		return domain.WorkHours{}, nil, err
	}
	values, err := tx.ListRuns(ctx)
	if err != nil {
		return domain.WorkHours{}, nil, err
	}
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return domain.WorkHours{}, nil, err
	}
	var runs []domain.WorkHoursRun
	var excluded []excludedRun
	for _, value := range values {
		if request.ProjectID != nil && value.Value.ProjectID != *request.ProjectID {
			continue
		}
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return domain.WorkHours{}, nil, fmt.Errorf("run %q: %w", value.Value.ID, err)
		}
		run, err := workHoursRun(ctx, tx, state, value.Value, items)
		if errors.Is(err, ErrRunObservationIntegrity) {
			excluded = append(excluded, excludedRun{id: value.Value.ID, projectID: value.Value.ProjectID, err: err})
			continue
		}
		if err != nil {
			return domain.WorkHours{}, nil, fmt.Errorf("run %q: %w", value.Value.ID, err)
		}
		runs = append(runs, run)
	}
	plain := make([]domain.AttentionItem, 0, len(items))
	for _, item := range items {
		plain = append(plain, item.Value)
	}
	hours, err := domain.WorkHoursOf(domain.WorkHoursInput{
		Period: request.Period, AsOf: now().UTC(), ProjectID: request.ProjectID,
		HoldHistorySince: holdsSince, SkippedRunCount: len(excluded),
		Runs: runs, Items: plain,
	})
	if err != nil {
		return domain.WorkHours{}, nil, err
	}
	return hours, excluded, nil
}

// workHoursRun reads one run with the calls the timelines make
// (GetRunTimeline, loadedTaskTimeline): the authenticated observation, the
// projection facts that decide whether it is finished, and its hold spans.
func workHoursRun(
	ctx context.Context, tx *store.ReadTx, state store.ServerState, run domain.Run,
	items []store.Snapshotted[domain.AttentionItem],
) (domain.WorkHoursRun, error) {
	observation, err := observeAuthenticatedRun(ctx, tx, state, run, items)
	if err != nil {
		return domain.WorkHoursRun{}, err
	}
	facts, err := runProjectionFactsFor(ctx, tx, run, observation)
	if err != nil {
		return domain.WorkHoursRun{}, asRunObservationIntegrityError(err)
	}
	if err := readTimelineTimeFacts(ctx, tx, run, observation, &facts); err != nil {
		return domain.WorkHoursRun{}, err
	}
	return domain.WorkHoursRun{
		RunID: run.ID, TaskID: run.TaskID, Milestones: observation.Milestones,
		Finished: facts.finished, Holds: facts.holdHistory,
	}, nil
}
