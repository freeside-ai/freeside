package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (w *productionPublicationWorkflow) reviewYieldHistory(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewYieldHistory, error) {
	var records []domain.ReviewRecord
	var dispositions []domain.ReviewDispositionRecord
	findings := map[domain.FindingID]domain.Finding{}
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		records, err = tx.ListReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		dispositions, err = tx.ListFindingDispositions(ctx, runID)
		if err != nil {
			return err
		}
		for _, record := range records {
			for _, id := range record.FindingIDs {
				if _, ok := findings[id]; ok {
					continue
				}
				finding, err := tx.GetFinding(ctx, id)
				if err != nil {
					return err
				}
				findings[id] = finding
			}
		}
		return nil
	}); err != nil {
		return domain.ReviewYieldHistory{}, fmt.Errorf("load review yield history %q: %w", runID, err)
	}
	history, err := deriveReviewYieldHistory(records, dispositions, findings)
	if err != nil {
		return domain.ReviewYieldHistory{}, fmt.Errorf("derive review yield history %q: %w", runID, err)
	}
	return history, nil
}

func deriveReviewYieldHistory(
	records []domain.ReviewRecord,
	dispositions []domain.ReviewDispositionRecord,
	findings map[domain.FindingID]domain.Finding,
) (domain.ReviewYieldHistory, error) {
	dispositionsByRound := make(map[int][]domain.ReviewDispositionRecord)
	recordedRounds := make(map[int]struct{}, len(records))
	for _, record := range records {
		recordedRounds[record.Round] = struct{}{}
	}
	for _, disposition := range dispositions {
		if _, ok := recordedRounds[disposition.Round]; !ok {
			return domain.ReviewYieldHistory{}, fmt.Errorf(
				"disposition for absent round %d: %w",
				disposition.Round, domain.ErrReviewYieldHistoryInconsistent)
		}
		dispositionsByRound[disposition.Round] = append(
			dispositionsByRound[disposition.Round], disposition)
	}

	seen := map[domain.FindingFingerprint]struct{}{}
	rounds := make([]domain.ReviewYieldRound, 0, len(records))
	var segmentConfiguration domain.Digest
	for _, record := range records {
		if len(rounds) == 0 || record.ConfigurationDigest != segmentConfiguration {
			clear(seen)
			segmentConfiguration = record.ConfigurationDigest
		}
		round := domain.ReviewYieldRound{
			Round: record.Round, FindingsIngested: len(record.FindingIDs), Outcome: record.Outcome,
		}
		current := make([]domain.FindingFingerprint, 0, len(record.FindingIDs))
		for _, id := range record.FindingIDs {
			finding, ok := findings[id]
			if !ok {
				return domain.ReviewYieldHistory{}, fmt.Errorf(
					"round %d finding %q absent: %w",
					record.Round, id, domain.ErrReviewYieldHistoryInconsistent)
			}
			fingerprint, err := finding.Fingerprint()
			if errors.Is(err, domain.ErrUnfingerprintableFinding) {
				// Recurrence needs positive cross-round identity. A finding that
				// cannot supply one may still be declined or deferred, so count the
				// occurrence as new without letting it block the ready path.
				round.NewFindings++
				continue
			}
			if err != nil {
				return domain.ReviewYieldHistory{}, fmt.Errorf(
					"round %d finding %q: %w", record.Round, id, err)
			}
			current = append(current, fingerprint)
			if _, recurring := seen[fingerprint]; recurring {
				round.RecurringFindings++
			} else {
				round.NewFindings++
			}
		}
		for _, fingerprint := range current {
			seen[fingerprint] = struct{}{}
		}
		for _, disposition := range dispositionsByRound[record.Round] {
			switch disposition.Disposition {
			case domain.ReviewDispositionFixed:
				round.Fixed++
			case domain.ReviewDispositionDeclined:
				round.Declined++
			case domain.ReviewDispositionDeferred:
				round.Deferred++
			}
		}
		rounds = append(rounds, round)
	}
	terminal := domain.ReviewOutcome("")
	if len(records) > 0 {
		terminal = records[len(records)-1].Outcome
	}
	return domain.NewReviewYieldHistory(domain.ReviewYieldHistory{
		Rounds: rounds, TerminalOutcome: terminal,
	})
}
