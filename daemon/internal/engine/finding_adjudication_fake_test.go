package engine

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// deterministicFindingAdjudicator is the stage's contract fake. It scripts
// one immutable model entry per finding ID and returns entries in request
// order, so route fixtures are deterministic and a missing script exercises
// the production not-accepted park.
type deterministicFindingAdjudicator struct {
	scripts map[domain.FindingID]domain.FindingAdjudicationEntry
}

func newDeterministicFindingAdjudicator(
	entries ...domain.FindingAdjudicationEntry,
) *deterministicFindingAdjudicator {
	scripts := make(map[domain.FindingID]domain.FindingAdjudicationEntry, len(entries))
	for _, entry := range entries {
		scripts[entry.FindingID] = entry
	}
	return &deterministicFindingAdjudicator{scripts: scripts}
}

func (f *deterministicFindingAdjudicator) Adjudicate(
	_ context.Context, request findingAdjudicationRequest,
) ([]domain.FindingAdjudicationEntry, error) {
	entries := make([]domain.FindingAdjudicationEntry, 0, len(request.Findings))
	for _, input := range request.Findings {
		entry, ok := f.scripts[input.Finding.ID]
		if !ok {
			return nil, nil
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
