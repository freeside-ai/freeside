package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putRecommendationSourceSQL = `
INSERT INTO attention_recommendation_sources (
    item_id, digest, source, decision_surface_digest, body
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (digest) DO NOTHING`

// PutRecommendationSource persists one immutable daemon-written source record.
// The deferred item foreign key lets a producer insert a record immediately
// before PutAttentionItem in the same transaction, so creation can derive the
// recommendation from the complete persisted set.
func (tx *WriteTx) PutRecommendationSource(
	ctx context.Context, record domain.RecommendationSourceRecord,
) error {
	body, err := encode(record)
	if err != nil {
		return fmt.Errorf("put recommendation source %q: %w", record.Digest, err)
	}
	if err := tx.putImmutable(
		ctx, putRecommendationSourceSQL,
		[]any{record.ItemID, record.Digest, record.Source, record.DecisionSurfaceDigest, body},
		`SELECT body FROM attention_recommendation_sources WHERE digest = ?`,
		[]any{record.Digest}, body,
	); err != nil {
		return fmt.Errorf("put recommendation source %q: %w", record.Digest, err)
	}
	return nil
}

type recommendationSourceRow struct {
	itemID                string
	digest                string
	source                string
	decisionSurfaceDigest string
	body                  []byte
}

// ListRecommendationSources enumerates and re-gates every source row before
// filtering to itemID. Enumerating the table prevents a forged copied item_id
// column from hiding a corrupt record from every keyed read.
func (tx *ReadTx) ListRecommendationSources(
	ctx context.Context, itemID domain.ItemID,
) ([]domain.RecommendationSourceRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT item_id, digest, source,
        decision_surface_digest, body
        FROM attention_recommendation_sources ORDER BY digest`)
	if err != nil {
		return nil, fmt.Errorf("list recommendation sources %q: %w", itemID, err)
	}
	var raw []recommendationSourceRow
	for rows.Next() {
		var row recommendationSourceRow
		if err := rows.Scan(&row.itemID, &row.digest, &row.source, &row.decisionSurfaceDigest, &row.body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list recommendation sources %q: %w", itemID, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list recommendation sources %q: %w", itemID, err)
	}
	out := make([]domain.RecommendationSourceRecord, 0, len(raw))
	for i, row := range raw {
		record, err := decode[domain.RecommendationSourceRecord](row.body)
		if err != nil {
			return nil, fmt.Errorf("list recommendation sources %q row %d: %w", itemID, i+1, err)
		}
		if string(record.ItemID) != row.itemID || string(record.Digest) != row.digest ||
			string(record.Source) != row.source ||
			string(record.DecisionSurfaceDigest) != row.decisionSurfaceDigest {
			return nil, fmt.Errorf("list recommendation sources %q row %d: %w", itemID, i+1, errRowInconsistent)
		}
		if record.ItemID == itemID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (tx *ReadTx) deriveRecommendation(
	ctx context.Context, item domain.AttentionItem, surface domain.DecisionSurface,
) (*domain.Recommendation, error) {
	records, err := tx.ListRecommendationSources(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	return domain.DeriveRecommendation(item, surface, records, recommendationAuthority{ctx: ctx, tx: tx})
}

// gateRecommendation suppresses only a recommendation whose stored projection
// no longer equals the current derivation. The containing item and action set
// remain readable.
func (tx *ReadTx) gateRecommendation(ctx context.Context, item *domain.AttentionItem) {
	surface, err := tx.DecisionSurface(ctx, item.ID)
	if err != nil {
		item.Recommendation = nil
		return
	}
	derived, err := tx.deriveRecommendation(ctx, *item, surface)
	if err != nil || !reflect.DeepEqual(item.Recommendation, derived) {
		item.Recommendation = nil
	}
}

type recommendationAuthority struct {
	ctx context.Context
	tx  *ReadTx
}

func (a recommendationAuthority) ResolveAgentJudgment(
	site domain.JudgmentSite, invocationID domain.InvocationID, artifactDigest domain.Digest,
) (domain.AgentJudgmentRecommendation, error) {
	if site != domain.JudgmentSiteFindingAdjudicator {
		return domain.AgentJudgmentRecommendation{}, domain.ErrInvalidJudgmentSite
	}
	artifact, err := a.tx.GetFindingAdjudication(a.ctx, artifactDigest)
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return domain.AgentJudgmentRecommendation{}, domain.ErrParentKeyMismatch
	}
	if err != nil {
		return domain.AgentJudgmentRecommendation{}, err
	}
	record, err := a.tx.reviewRecordForRound(a.ctx, artifact.RunID, artifact.Round)
	if err != nil {
		return domain.AgentJudgmentRecommendation{}, err
	}
	if record.InvocationID != invocationID {
		return domain.AgentJudgmentRecommendation{}, domain.ErrParentKeyMismatch
	}
	return domain.AgentJudgmentRecommendation{
		RunID: artifact.RunID, Round: artifact.Round,
		DecisionSurfaceDigest: artifact.DecisionSurfaceDigest,
		Projection: domain.RecommendationProjection{
			Action: domain.ActionAcceptRecommendedRoute,
			Reason: domain.FindingAdjudicatorRecommendationReason,
		},
	}, nil
}

func (a recommendationAuthority) DaemonPolicyRule(
	ruleDigest domain.Digest,
) (domain.DaemonPolicyRule, bool) {
	rule, ok := a.tx.recommendationRules[ruleDigest]
	return rule, ok
}

func (a recommendationAuthority) CurrentResolvedPolicyDigest(
	runID domain.RunID,
) (domain.Digest, error) {
	policy, err := a.tx.GetResolvedPolicy(a.ctx, runID)
	if err != nil {
		return "", err
	}
	return policy.Digest, nil
}
