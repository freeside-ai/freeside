package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// DeriveReviewYieldHistory reconstructs the immutable per-round yield digest
// from validated review rows. Keeping the pure derivation at the persistence
// boundary lets both the engine and command-authority re-gates use one
// definition instead of trusting a caller-supplied history.
func DeriveReviewYieldHistory(
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
				// cannot supply one remains new for convergence purposes.
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

// ReviewYieldHistory reconstructs the current run history from validated rows.
func (tx *ReadTx) ReviewYieldHistory(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewYieldHistory, error) {
	return tx.reviewYieldHistory(ctx, runID, 0, false)
}

// ReviewYieldHistoryAtDecision reconstructs what a diminishing-review item
// displayed when it was created: review records through round are included,
// while that round's later action-generated dispositions are not.
func (tx *ReadTx) ReviewYieldHistoryAtDecision(
	ctx context.Context, runID domain.RunID, round int,
) (domain.ReviewYieldHistory, error) {
	if round < 1 {
		return domain.ReviewYieldHistory{}, domain.ErrNonPositive
	}
	return tx.reviewYieldHistory(ctx, runID, round, true)
}

func (tx *ReadTx) reviewYieldHistory(
	ctx context.Context, runID domain.RunID, throughRound int, excludeTerminalDispositions bool,
) (domain.ReviewYieldHistory, error) {
	records, err := tx.ListReviewRecords(ctx, runID)
	if err != nil {
		return domain.ReviewYieldHistory{}, err
	}
	if throughRound > 0 {
		selected := records[:0:0]
		for _, record := range records {
			if record.Round <= throughRound {
				selected = append(selected, record)
			}
		}
		records = selected
	}
	var dispositions []domain.ReviewDispositionRecord
	if throughRound > 0 && excludeTerminalDispositions {
		dispositions, err = tx.loadFindingDispositionsAtDecision(ctx, runID, throughRound)
	} else {
		dispositions, err = tx.ListFindingDispositions(ctx, runID)
	}
	if err != nil {
		return domain.ReviewYieldHistory{}, err
	}
	if throughRound > 0 {
		selected := dispositions[:0:0]
		for _, disposition := range dispositions {
			if disposition.RunID == runID && disposition.Round <= throughRound {
				selected = append(selected, disposition)
			}
		}
		dispositions = selected
	}
	findings := make(map[domain.FindingID]domain.Finding)
	for _, record := range records {
		for _, id := range record.FindingIDs {
			if _, ok := findings[id]; ok {
				continue
			}
			finding, err := tx.GetFinding(ctx, id)
			if err != nil {
				return domain.ReviewYieldHistory{}, err
			}
			findings[id] = finding
		}
	}
	return DeriveReviewYieldHistory(records, dispositions, findings)
}
