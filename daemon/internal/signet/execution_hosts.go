package signet

import (
	"context"
	"fmt"
	"net/http"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ExecutionHostResponse is one entry of GET /execution-hosts
// (api/openapi.yaml ExecutionHost). It carries no reach and no count
// override: a client needs neither, and the host key stays in the daemon.
type ExecutionHostResponse struct {
	ID         domain.ExecutionHostID         `json:"id"`
	Name       string                         `json:"name"`
	RunAccount domain.HostRunAccount          `json:"run_account"`
	Conditions []domain.HostObservedCondition `json:"conditions"`
}

// ListExecutionHosts reads every execution host, ordered by id, in one read
// transaction. It writes nothing.
func (s *Service) ListExecutionHosts(ctx context.Context) ([]ExecutionHostResponse, error) {
	var hosts []domain.ExecutionHost
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		hosts, err = tx.ListExecutionHosts(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("list execution hosts: %w", err)
	}
	out := make([]ExecutionHostResponse, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, ExecutionHostResponse{
			ID: host.ID, Name: host.Name, RunAccount: host.RunAccount, Conditions: host.Conditions,
		})
	}
	return out, nil
}

func (h httpHandler) listExecutionHosts(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	hosts, err := h.service.ListExecutionHosts(r.Context())
	if err != nil {
		// A host read finds every row it lists, so any failure is the
		// daemon's, never a missing resource.
		writeJSON(w, http.StatusInternalServerError, errorResponse{Message: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}
