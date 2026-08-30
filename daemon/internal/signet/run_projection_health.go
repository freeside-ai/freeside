package signet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runProjectionHealthIDPrefix keys the deterministic identity of the durable
// health item a listing read mints when it excludes a run for a projection
// integrity contradiction (#767, #770). The run id completes the identity: one
// item per run, so a re-read of the same damaged run converges on the same item
// rather than accumulating duplicates, and an operator acknowledge or a clean
// re-projection's resolve stays terminal instead of re-surfacing on the next
// read. The accepted tradeoff of a run-singleton (rather than episode-scoped)
// id: a run re-damaged after its item already resolved mints nothing new (the
// version-1 mint collides with the dead terminal id, so PutAttentionItem
// rejects it and the converge logs, not raises). Accepted because a repaired
// legacy row does not re-corrupt, and a run-singleton id also keeps an
// acknowledge from re-surfacing while the run is still damaged, which an
// episode-scoped id would not.
const runProjectionHealthIDPrefix = "system-health-run-projection-"

// runProjectionHealthID is the deterministic item id for one excluded run.
func runProjectionHealthID(runID domain.RunID) domain.ItemID {
	return domain.ItemID(runProjectionHealthIDPrefix + string(runID))
}

// convergeRunProjectionHealth surfaces every run a full listing read excluded
// for a projection integrity contradiction (#767) as a durable, operator-facing
// AttentionSystemHealth item (#770), and resolves any previously-minted item
// whose run now projects cleanly (or no longer exists). It runs only on the
// full-listing reads (Bootstrap, ListRuns), whose excluded set is the complete
// set of currently-damaged runs, after the read transaction closes.
//
// Best-effort by construction: a mint or resolve is applied in its own write,
// and a failure is logged and swallowed rather than failing the served read.
// Failing the read would recreate the #767 whole-surface outage this seam
// exists to prevent; per-item writes keep one damaged run's write failure from
// suppressing another's item, mirroring #767's per-run isolation. The next
// listing read retries the converge.
func (s *Service) convergeRunProjectionHealth(ctx context.Context, excluded []excludedRun) {
	var open []domain.AttentionItem
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		open = runProjectionHealthOpenItems(items)
		return nil
	}); err != nil {
		s.logger.Warn("run projection health converge: read open items", "error", err)
		return
	}
	// The healthy steady state: nothing was excluded and no item is open, so
	// there is no mint or resolve to do and no write is opened.
	if len(excluded) == 0 && len(open) == 0 {
		return
	}

	excludedRuns := make(map[domain.RunID]bool, len(excluded))
	openRuns := make(map[domain.RunID]bool, len(open))
	for _, item := range open {
		if item.Subject.RunID != nil {
			openRuns[*item.Subject.RunID] = true
		}
	}

	// Mint one advisory item per still-damaged run not already surfaced. A run
	// whose item is already open is left untouched, so a repeated read causes no
	// item_version churn (PutAttentionItem would converge a byte-identical write,
	// but skipping the write avoids even that work).
	for _, run := range excluded {
		excludedRuns[run.id] = true
		if openRuns[run.id] {
			continue
		}
		var names *domain.DisplayNames
		if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			names, err = tx.DisplayNamesFor(ctx, run.projectID, domain.Subject{
				Type: domain.SubjectRun, ID: domain.SubjectID(run.id), RunID: &run.id,
			})
			return err
		}); err != nil {
			s.logger.Warn("run projection health converge: derive display names",
				"run", run.id, "error", err)
			continue
		}
		item, err := newRunProjectionHealthItem(run, s.now().UTC(), names)
		if err != nil {
			s.logger.Warn("run projection health converge: build item",
				"run", run.id, "error", err)
			continue
		}
		if err := s.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(ctx, item)
		}); err != nil {
			s.logger.Warn("run projection health converge: mint item",
				"run", run.id, "error", err)
		}
	}

	// Resolve any open item whose run projected cleanly this pass or no longer
	// exists. The excluded set is complete for a full listing, so a run absent
	// from it is no longer damaged.
	for _, item := range open {
		if item.Subject.RunID != nil && excludedRuns[*item.Subject.RunID] {
			continue
		}
		resolved := item
		resolved.ItemVersion++
		resolved.Status = domain.StatusResolved
		if err := s.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutAttentionItem(ctx, resolved)
		}); err != nil {
			s.logger.Warn("run projection health converge: resolve item",
				"item", item.ID, "error", err)
		}
	}
}

// runProjectionHealthOpenItems narrows a set of open system_health items to the
// run-projection items this converge owns, keyed by the deterministic prefix.
func runProjectionHealthOpenItems(items []domain.AttentionItem) []domain.AttentionItem {
	var out []domain.AttentionItem
	for _, item := range items {
		if strings.HasPrefix(string(item.ID), runProjectionHealthIDPrefix) {
			out = append(out, item)
		}
	}
	return out
}

// newRunProjectionHealthItem builds the advisory item for one excluded run. It
// binds to the run as {SubjectRun, SubjectID(runID), RunID, run's ProjectID} so
// the item passes authenticateRunObservation's run-binding check rather than
// itself reading as a new integrity failure; the untrusted contradiction text
// is deliberately not embedded, only the trusted run coordinate the operator
// inspects. Advisory posture surfaces the damaged run without gating unattended
// admission for the whole system (a blocking posture would).
func newRunProjectionHealthItem(
	run excludedRun, createdAt time.Time, displayNames *domain.DisplayNames,
) (domain.AttentionItem, error) {
	runID := run.id
	posture := domain.HealthPostureAdvisory
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        runProjectionHealthID(runID),
		ProjectID: run.projectID,
		Subject: domain.Subject{
			Type:  domain.SubjectRun,
			ID:    domain.SubjectID(runID),
			RunID: &runID,
		},
		Type:     domain.AttentionSystemHealth,
		Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Run %s is excluded from listings: its observation projection "+
				"contradicts durable authority and cannot be served. "+
				"Inspect the daemon logs for the integrity contradiction.",
			runID),
		RequestedDecision: []domain.Action{domain.ActionRunDoctor, domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "run_projection_contradiction", Impairs: domain.ImpairedCapabilityRunVisibility,
		},
		DisplayNames:      displayNames,
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt,
		Posture:           &posture,
		Status:            domain.StatusOpen,
	}, nil)
}
