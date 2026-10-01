package store

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// EffectiveFindingDisposition is one finding's latest stored disposition as of
// a round, with the disposition that holds once drift reversals are applied.
// Supersession is the record that changed it, and nil when none did.
type EffectiveFindingDisposition struct {
	Stored       domain.ReviewDispositionRecord
	Effective    domain.ReviewDisposition
	Supersession *domain.FindingDispositionSupersession
}

// EffectiveFindingDispositions returns each finding's effective latest
// disposition as of throughRound, in finding order (plan §7 Review Drift). A
// finding's latest disposition is its stored row with the highest round at or
// before throughRound; a finding listed by several rounds has several rows. It
// reads as declined when a supersession record targets that row and its
// reversing round is at or before throughRound, so a reversed fix reads as
// fixed before its reversing round and declined from it on. A record that
// targets an older row of the finding changes nothing: the later disposition
// stands.
func (tx *ReadTx) EffectiveFindingDispositions(
	ctx context.Context, runID domain.RunID, throughRound int,
) ([]EffectiveFindingDisposition, error) {
	if throughRound < 1 {
		return nil, fmt.Errorf("effective finding dispositions %q through round %d: %w",
			runID, throughRound, domain.ErrNonPositive)
	}
	dispositions, err := tx.ListFindingDispositions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("effective finding dispositions %q: %w", runID, err)
	}
	supersessions, err := tx.ListFindingDispositionSupersessions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("effective finding dispositions %q: %w", runID, err)
	}
	latest := make(map[domain.FindingID]domain.ReviewDispositionRecord)
	for _, disposition := range dispositions {
		if disposition.Round > throughRound {
			continue
		}
		if current, ok := latest[disposition.FindingID]; !ok || disposition.Round > current.Round {
			latest[disposition.FindingID] = disposition
		}
	}
	out := make([]EffectiveFindingDisposition, 0, len(latest))
	for _, stored := range latest {
		effective := EffectiveFindingDisposition{Stored: stored, Effective: stored.Disposition}
		for _, supersession := range supersessions {
			if supersession.FindingID == stored.FindingID && supersession.SupersededRound == stored.Round &&
				supersession.ReversingRound <= throughRound {
				effective.Effective = domain.ReviewDispositionDeclined
				effective.Supersession = &supersession
			}
		}
		out = append(out, effective)
	}
	slices.SortFunc(out, func(a, b EffectiveFindingDisposition) int {
		return strings.Compare(string(a.Stored.FindingID), string(b.Stored.FindingID))
	})
	return out, nil
}
