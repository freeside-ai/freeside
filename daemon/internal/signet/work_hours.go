package signet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
	// The read bounds days in a zone the client names. LoadLocation reads
	// the host's zone database first; the embedded copy answers on a host
	// without one, such as a minimal container.
	_ "time/tzdata"

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

// getWorkHours serves GET /work-hours. Each query parameter is checked where
// it enters and grants nothing; a refusal names the parameter.
func (h httpHandler) getWorkHours(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	request, err := parseWorkHoursRequest(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: err.Error()})
		return
	}
	hours, err := h.service.WorkHours(r.Context(), request)
	if errors.Is(err, ErrWorkHoursProjectNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Message: "not found"})
		return
	}
	if err != nil {
		// Not writeReadError: any other record the read can't find is a
		// broken reference, never a missing resource, so it is not a 404.
		writeJSON(w, http.StatusInternalServerError, errorResponse{Message: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, hours)
}

func parseWorkHoursRequest(query url.Values) (WorkHoursRequest, error) {
	from, err := requiredQueryValue(query, "from")
	if err != nil {
		return WorkHoursRequest{}, err
	}
	fromDate, err := time.Parse(time.DateOnly, from)
	if err != nil || fromDate.Year() < 1 {
		return WorkHoursRequest{}, errors.New("from must be a calendar date, YYYY-MM-DD")
	}
	rawDays, err := requiredQueryValue(query, "days")
	if err != nil {
		return WorkHoursRequest{}, err
	}
	days, err := strconv.Atoi(rawDays)
	if err != nil || days < 1 || days > domain.MaxWorkHoursDays {
		return WorkHoursRequest{}, fmt.Errorf("days must be an integer from 1 to %d", domain.MaxWorkHoursDays)
	}
	if !domain.WorkHoursPeriodInRange(fromDate, days) {
		return WorkHoursRequest{}, errors.New("the period must end by 9999-12-30")
	}
	zone, err := requiredQueryValue(query, "time_zone")
	if err != nil {
		return WorkHoursRequest{}, err
	}
	// LoadLocation reads "" as UTC and "Local" as the host's zone; the
	// reader must name a zone, and the host's never applies.
	location, err := time.LoadLocation(zone)
	if zone == "Local" || err != nil {
		return WorkHoursRequest{}, fmt.Errorf("time_zone %q is not a known IANA time zone", zone)
	}
	request := WorkHoursRequest{
		Period: domain.WorkHoursPeriod{From: fromDate, Days: days, Location: location},
	}
	if _, present := query["project_id"]; present {
		project, err := requiredQueryValue(query, "project_id")
		if err != nil {
			return WorkHoursRequest{}, err
		}
		request.ProjectID = new(domain.ProjectID(project))
	}
	return request, nil
}

// requiredQueryValue returns the one non-empty value of name, refusing it
// missing, empty, or repeated.
func requiredQueryValue(query url.Values, name string) (string, error) {
	values := query[name]
	switch {
	case len(values) == 0:
		return "", fmt.Errorf("%s is required", name)
	case len(values) > 1:
		return "", fmt.Errorf("%s must be given once", name)
	case values[0] == "":
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return values[0], nil
}
