// Package observedb is the whole of the follow path's access to the daemon's
// database: open it, read one run's bounded supervision snapshot, close it.
// Nothing else is exported, so the follow view cannot reach the store's write,
// checkpoint, restore, or backup-file surfaces even though this package
// imports the store to build on them.
//
// Its own proof is not another assertion: it is that this file is short
// enough to read in full and exports exactly three things. An import
// allowlist bounds which packages a caller can name, never which methods of
// a permitted package it calls, so the regress has to stop at a surface
// small enough to check by eye. This is that surface, and
// internal/observe/containment_test.go says so rather than claiming its
// allowlist is total.
package observedb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
)

// Store is a read-only view of one daemon database's run observations.
type Store struct {
	store *store.Store
}

// AttentionItem is the bounded, non-prose part of an open AttentionItem that
// an operator needs to identify and resume one exact run. Reasons, evidence,
// claims, and conversations are deliberately absent: the observation command
// is a diagnostic identity surface, not an alternate attention-card renderer.
type AttentionItem struct {
	ID                          domain.ItemID                              `json:"id"`
	Type                        domain.AttentionType                       `json:"type"`
	Status                      domain.ItemStatus                          `json:"status"`
	CreatedAt                   *time.Time                                 `json:"created_at,omitempty"`
	RequestedDecision           []domain.Action                            `json:"requested_decision"`
	ReviewConfigurationRecovery *domain.ReviewConfigurationRecoveryBinding `json:"review_configuration_recovery,omitempty"`
}

// Admission is the immutable identity subset of one recorded execution
// admission. It omits workspace and auth identity fields, which are not
// needed to diagnose the production lifecycle.
type Admission struct {
	InvocationID              domain.InvocationID `json:"invocation_id"`
	Stage                     string              `json:"stage"`
	ImageRef                  domain.ImageRef     `json:"image_ref"`
	ImageDigest               domain.Digest       `json:"image_digest"`
	Base                      domain.BaseRevision `json:"base"`
	TrustProfileDigest        *domain.Digest      `json:"trust_profile_digest,omitempty"`
	ReviewConfigurationDigest *domain.Digest      `json:"review_configuration_digest,omitempty"`
}

// Lineage is the identity-only projection of a production attempt. The
// admitted publication document is intentionally absent: its content is not
// needed to resume a run and may contain arbitrary operator prose.
type Lineage struct {
	CampaignID          domain.CampaignID            `json:"campaign_id"`
	AttemptNumber       int                          `json:"attempt_number"`
	Kind                domain.ProductionAttemptKind `json:"kind"`
	ParentRunID         domain.RunID                 `json:"parent_run_id,omitempty"`
	SourceDigest        domain.Digest                `json:"source_digest"`
	PublicationDigest   domain.Digest                `json:"publication_digest,omitempty"`
	ApprovedSpecDigest  domain.Digest                `json:"approved_spec_digest,omitempty"`
	ElaborationRunID    domain.RunID                 `json:"elaboration_run_id"`
	ImplementationRunID domain.RunID                 `json:"implementation_run_id"`
}

// ReviewYield is the bounded telemetry projection for one completed review
// round. It carries only counters and durable decision identity, never finding
// prose or the attention item's human-facing reason.
type ReviewYield struct {
	AttemptNumber       int                  `json:"attempt_number"`
	Round               int                  `json:"round"`
	ConfigurationDigest domain.Digest        `json:"configuration_digest"`
	FindingsIngested    int                  `json:"findings_ingested"`
	NewFindings         int                  `json:"new_findings"`
	RecurringFindings   int                  `json:"recurring_findings"`
	Fixed               int                  `json:"fixed"`
	Declined            int                  `json:"declined"`
	Deferred            int                  `json:"deferred"`
	Outcome             domain.ReviewOutcome `json:"outcome"`
	DecisionAction      *domain.Action       `json:"decision_action,omitempty"`
}

// Snapshot is one transactionally coherent, read-only view of the selected
// run's observation, production-attempt lineage, open attention, recorded
// admission identities, and authenticated publication-completion identity.
type Snapshot struct {
	Observation             domain.RunObservation             `json:"observation"`
	LastStage               string                            `json:"-"`
	PublicationInvocationID domain.InvocationID               `json:"-"`
	Attempt                 *Lineage                          `json:"lineage,omitempty"`
	AttentionItems          []AttentionItem                   `json:"attention_items"`
	Admissions              []Admission                       `json:"admissions"`
	ShadowReviews           []domain.ShadowReviewRecord       `json:"shadow_reviews"`
	ClassifierSamples       []domain.ClassifierAccuracySample `json:"classifier_samples"`
	ReviewYield             []ReviewYield                     `json:"review_yield"`
}

// Open opens the daemon's database at path. Options are empty by design: the
// observation read surface re-validates every row it returns. It reads
// immutable admission records without applying mutable execution policy and
// excludes ready-item evidence from actionable-attention classification.
// Ordinary following therefore needs no policy input; a supervision snapshot
// may supply the run's approved recipe so an evidence-bearing blocked item is
// reconstructed through the same fail-closed gate as every other reader.
// Opening migrates the schema to head, the same behaviour freesided submit and
// doctor already have on this transport.
func Open(ctx context.Context, path string, approvedRecipes ...domain.Digest) (*Store, error) {
	approved := make(map[domain.Digest]bool, len(approvedRecipes))
	for _, recipe := range approvedRecipes {
		approved[recipe] = true
	}
	st, _, err := topicstore.Open(ctx, path, store.Options{ApprovedRecipes: approved})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return &Store{store: st}, nil
}

// ObserveRun reads one run's observation aggregate, re-validated by the store.
func (s *Store) ObserveRun(
	ctx context.Context, runID domain.RunID,
) (domain.RunObservation, error) {
	var observation domain.RunObservation
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		observation, err = tx.ObserveRun(ctx, runID)
		return err
	}); err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run: %w", err)
	}
	return observation, nil
}

// ObserveSnapshot reads one coherent supervision snapshot. Every source row
// is reconstructed by the store before its bounded projection is returned;
// no raw SQLite handle or mutable store capability crosses this package.
func (s *Store) ObserveSnapshot(ctx context.Context, runID domain.RunID) (Snapshot, error) {
	var snapshot Snapshot
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		observation, err := tx.ObserveRun(ctx, runID)
		if err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		attemptNumber := run.AttemptNumber
		if attempt, attemptErr := tx.GetProductionAttemptByRun(ctx, runID); attemptErr == nil {
			attemptNumber = attempt.AttemptNumber
		} else if !errors.Is(attemptErr, store.ErrNotFound) {
			return attemptErr
		}
		snapshot.Observation = observation
		snapshot.LastStage = lastStage(run)
		publicationInvocationID, completed, err := engine.ProductionPublicationCompletion(ctx, tx, run)
		if err != nil {
			return err
		}
		if completed {
			snapshot.PublicationInvocationID = publicationInvocationID
		}
		snapshot.AttentionItems = []AttentionItem{}
		snapshot.Admissions = []Admission{}
		snapshot.ShadowReviews = []domain.ShadowReviewRecord{}
		snapshot.ClassifierSamples = []domain.ClassifierAccuracySample{}
		snapshot.ReviewYield = []ReviewYield{}
		snapshot.ShadowReviews, err = tx.ListShadowReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		snapshot.ClassifierSamples, err = tx.ListClassifierAccuracySamples(ctx, runID)
		if err != nil {
			return err
		}
		reviewRecords, err := tx.ListReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		if len(reviewRecords) > 0 {
			history, err := tx.ReviewYieldHistory(ctx, runID)
			if err != nil {
				return err
			}
			decisions, err := tx.ListReviewDiminishingDecisions(ctx, runID)
			if err != nil {
				return err
			}
			decisionActions := make(map[int]domain.Action, len(decisions))
			for _, decision := range decisions {
				if decision.Command != nil {
					decisionActions[decision.Binding.Round] = decision.Command.Action
				}
			}
			for index, round := range history.Rounds {
				if index >= len(reviewRecords) || reviewRecords[index].Round != round.Round {
					return domain.ErrReviewYieldHistoryInconsistent
				}
				projected := ReviewYield{
					AttemptNumber: attemptNumber, Round: round.Round,
					ConfigurationDigest: reviewRecords[index].ConfigurationDigest,
					FindingsIngested:    round.FindingsIngested, NewFindings: round.NewFindings,
					RecurringFindings: round.RecurringFindings, Fixed: round.Fixed,
					Declined: round.Declined, Deferred: round.Deferred, Outcome: round.Outcome,
				}
				if action, ok := decisionActions[round.Round]; ok {
					action := action
					projected.DecisionAction = &action
				}
				snapshot.ReviewYield = append(snapshot.ReviewYield, projected)
			}
		}

		if run.CampaignID != "" {
			attempt, err := tx.GetProductionAttempt(ctx, run.CampaignID, run.AttemptNumber)
			if err != nil {
				return err
			}
			snapshot.Attempt = &Lineage{
				CampaignID: attempt.CampaignID, AttemptNumber: attempt.AttemptNumber,
				Kind: attempt.Kind, ParentRunID: attempt.ParentRunID,
				SourceDigest: attempt.SourceDigest, PublicationDigest: attempt.PublicationDigest,
				ApprovedSpecDigest: attempt.ApprovedSpecDigest,
				ElaborationRunID:   attempt.ElaborationRunID, ImplementationRunID: attempt.ImplementationRunID,
			}
		}

		records, err := tx.ListOpenAttentionItemRecordsForRun(ctx, runID)
		if err != nil {
			return err
		}
		for _, record := range records {
			// A ready item accompanies the final published outcome and is not
			// an actionable hold. Excluding it also keeps historical evidence
			// recipes outside this current-run observation boundary.
			if record.Type == domain.AttentionReadyForFinalReview {
				continue
			}
			item, err := tx.GetAttentionItem(ctx, record.ID)
			if err != nil {
				return err
			}
			snapshot.AttentionItems = append(snapshot.AttentionItems, AttentionItem{
				ID: item.ID, Type: item.Type, Status: item.Status, CreatedAt: item.CreatedAt,
				RequestedDecision:           append([]domain.Action(nil), item.RequestedDecision...),
				ReviewConfigurationRecovery: item.ReviewConfigurationRecovery,
			})
		}
		sort.Slice(snapshot.AttentionItems, func(i, j int) bool {
			left, right := snapshot.AttentionItems[i], snapshot.AttentionItems[j]
			if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
				return left.CreatedAt.Before(*right.CreatedAt)
			}
			if left.CreatedAt == nil && right.CreatedAt != nil {
				return true
			}
			return left.ID < right.ID
		})

		for _, stage := range run.Stages {
			for _, runAttempt := range stage.Attempts {
				admission, err := tx.GetExecutionAdmissionRecord(ctx, runAttempt.InvocationID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if admission.RunID != run.ID || admission.StageID != stage.ID ||
					admission.AttemptID != runAttempt.ID {
					return fmt.Errorf("execution admission %q does not bind run %q stage %q attempt %q: %w",
						admission.InvocationID, run.ID, stage.ID, runAttempt.ID, domain.ErrParentKeyMismatch)
				}
				projected := Admission{
					InvocationID: admission.InvocationID, Stage: stage.Name,
					ImageRef: admission.ImageRef, ImageDigest: imageDigest(admission.ImageRef),
					Base: admission.Base, TrustProfileDigest: admission.TrustProfileDigest,
				}
				if admission.TrustProfileDigest != nil {
					profile, err := tx.GetTrustProfile(ctx, *admission.TrustProfileDigest)
					if err != nil {
						return err
					}
					digest := profile.Review.ConfigDigest
					projected.ReviewConfigurationDigest = &digest
				}
				snapshot.Admissions = append(snapshot.Admissions, projected)
			}
		}
		return nil
	}); err != nil {
		return Snapshot{}, fmt.Errorf("observe snapshot: %w", err)
	}
	return snapshot, nil
}

func imageDigest(ref domain.ImageRef) domain.Digest {
	if _, digest, ok := strings.Cut(string(ref), "@"); ok {
		return domain.Digest(digest)
	}
	return ""
}

func lastStage(run domain.Run) string {
	for i := len(run.Stages) - 1; i >= 0; i-- {
		if len(run.Stages[i].Attempts) > 0 {
			return run.Stages[i].Name
		}
	}
	return ""
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.store.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}
