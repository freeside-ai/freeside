package signet

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RestartReadinessResponse is the wire shape of GET /restart-readiness
// (api/openapi.yaml RestartReadiness).
type RestartReadinessResponse struct {
	Ready    bool                    `json:"ready"`
	Drain    *UpdateDrainResponse    `json:"drain"`
	Blockers []domain.RestartBlocker `json:"blockers"`
	AsOf     time.Time               `json:"as_of"`
}

// UpdateDrainResponse is the drain in force as a client sees it: when it
// began and whether it was forced. The build that wrote it stays in the
// daemon.
type UpdateDrainResponse struct {
	Since  time.Time `json:"since"`
	Forced bool      `json:"forced"`
}

// RestartReadiness runs the store's readiness read in one read transaction
// at the service clock, with the in-process work counted before it begins.
// It writes nothing.
//
// Counting first errs toward not ready: work that ends between the count and
// the read still reads as running, and work that starts after the count
// starts only after checking the drain, which a drained reader has already
// seen in force.
func (s *Service) RestartReadiness(ctx context.Context) (RestartReadinessResponse, error) {
	var running map[domain.RestartBlockerKind]int
	if s.runningWork != nil {
		running = s.runningWork()
	}
	var readiness domain.RestartReadiness
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		readiness, err = tx.RestartReadiness(ctx, s.now().UTC(), running)
		return err
	}); err != nil {
		return RestartReadinessResponse{}, fmt.Errorf("restart readiness: %w", err)
	}
	out := RestartReadinessResponse{
		Ready: readiness.Ready, Blockers: readiness.Blockers, AsOf: readiness.AsOf,
	}
	if readiness.Drain != nil {
		out.Drain = &UpdateDrainResponse{Since: readiness.Drain.OccurredAt, Forced: readiness.Drain.Forced}
	}
	return out, nil
}

func (h httpHandler) getRestartReadiness(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	readiness, err := h.service.RestartReadiness(r.Context())
	if err != nil {
		// Not writeReadError: a record the read can't find is a broken
		// reference, never a missing resource, so it is not a 404.
		writeJSON(w, http.StatusInternalServerError, errorResponse{Message: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, readiness)
}
