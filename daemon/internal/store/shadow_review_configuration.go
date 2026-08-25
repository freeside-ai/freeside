package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ShadowReviewConfigurationApprovalRecord pairs immutable owner authority
// with bookkeeping time. RecordedAt does not participate in ApprovalDigest.
type ShadowReviewConfigurationApprovalRecord struct {
	Approval   domain.ShadowReviewConfigurationApproval
	RecordedAt time.Time
}

// CurrentShadowReviewConfigurationApprovalInspection reconstructs the current
// activation for one repo/source. Deterministic absence or corruption is
// carried in ReconstructionError; query and scan failures are returned.
type CurrentShadowReviewConfigurationApprovalInspection struct {
	Repo                string
	RepositoryID        int64
	Source              domain.ShadowReviewSource
	Approval            domain.ShadowReviewConfigurationApproval
	ReconstructionError error
}

const (
	recordShadowReviewConfigurationApprovalSQL = `
INSERT INTO shadow_review_configuration_approvals (
    approval_digest, repo, repository_id, source, configuration_digest,
    recorded_at, body
)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (approval_digest) DO NOTHING`
	getShadowReviewConfigurationApprovalSQL = `
SELECT approval_digest, repo, repository_id, source, configuration_digest,
       recorded_at, body
FROM shadow_review_configuration_approvals
WHERE approval_digest = ?`
	recordShadowReviewConfigurationActivationSQL = `
INSERT INTO shadow_review_configuration_activations (
    repo, repository_id, source, approval_digest, configuration_digest,
    activated_at
)
VALUES (?, ?, ?, ?, ?, ?)`
	currentShadowReviewConfigurationApprovalSQL = `
SELECT a.repo, a.repository_id, a.source, a.approval_digest,
       a.configuration_digest,
       p.approval_digest, p.repo, p.repository_id, p.source,
       p.configuration_digest, p.recorded_at, p.body
FROM shadow_review_configuration_activations AS a
LEFT JOIN shadow_review_configuration_approvals AS p
  ON p.approval_digest = a.approval_digest
 AND p.repo = a.repo
 AND p.repository_id = a.repository_id
 AND p.source = a.source
 AND p.configuration_digest = a.configuration_digest
WHERE a.repo = ? AND a.source = ?
ORDER BY a.id DESC LIMIT 1`
)

// RecordInactiveShadowReviewConfigurationApproval persists reviewed content
// without selecting it. A byte-identical replay is inert and never revives an
// older approval after a later rotation.
func (tx *InternalTx) RecordInactiveShadowReviewConfigurationApproval(
	ctx context.Context,
	approval domain.ShadowReviewConfigurationApproval,
	recordedAt time.Time,
) error {
	body, err := encode(approval)
	if err != nil {
		return fmt.Errorf("record inactive shadow review configuration approval %q: %w",
			approval.Repo, err)
	}
	if recordedAt.IsZero() {
		return fmt.Errorf("record inactive shadow review configuration approval %q: zero recorded_at",
			approval.Repo)
	}
	if err := tx.putImmutable(
		ctx,
		recordShadowReviewConfigurationApprovalSQL,
		[]any{
			approval.ApprovalDigest, approval.Repo, approval.RepositoryID,
			approval.Source, approval.ConfigurationDigest,
			formatTime(recordedAt.UTC()), body,
		},
		`SELECT body FROM shadow_review_configuration_approvals WHERE approval_digest = ?`,
		[]any{approval.ApprovalDigest},
		body,
	); err != nil {
		return fmt.Errorf("record inactive shadow review configuration approval %q: %w",
			approval.Repo, err)
	}
	return nil
}

// ActivateShadowReviewConfigurationApproval explicitly selects a previously
// recorded approval. It re-resolves the current trust profile in this same
// transaction, so a stale repository ID cannot become active authority.
func (tx *InternalTx) ActivateShadowReviewConfigurationApproval(
	ctx context.Context,
	repo string,
	source domain.ShadowReviewSource,
	digest domain.Digest,
	activatedAt time.Time,
) error {
	if repo == "" || digest == "" {
		return errors.New("activate shadow review configuration approval: empty repo or digest")
	}
	if !slices.Contains(domain.AllShadowReviewSources, source) {
		return fmt.Errorf("activate shadow review configuration approval source %q: %w",
			source, domain.ErrInvalidShadowReviewSource)
	}
	if activatedAt.IsZero() {
		return fmt.Errorf("activate shadow review configuration approval %q: zero activated_at", repo)
	}
	approval, err := tx.GetShadowReviewConfigurationApproval(ctx, digest)
	if err != nil {
		return fmt.Errorf("activate shadow review configuration approval %q: %w", repo, err)
	}
	if approval.Repo != repo || approval.Source != source {
		return fmt.Errorf(
			"activate shadow review configuration approval %q/%s: digest belongs to %q/%s: %w",
			repo, source, approval.Repo, approval.Source,
			domain.ErrRepositoryIdentityMismatch,
		)
	}
	profile, err := tx.LatestTrustProfile(ctx, repo)
	if err != nil {
		return fmt.Errorf("activate shadow review configuration approval %q current trust profile: %w",
			repo, err)
	}
	if profile.Repo != approval.Repo || profile.RepositoryID != approval.RepositoryID {
		return fmt.Errorf(
			"activate shadow review configuration approval %q repository identity: %w",
			repo, domain.ErrRepositoryIdentityMismatch,
		)
	}
	if _, err := tx.tx.ExecContext(
		ctx,
		recordShadowReviewConfigurationActivationSQL,
		approval.Repo, approval.RepositoryID, approval.Source,
		approval.ApprovalDigest, approval.ConfigurationDigest,
		formatTime(activatedAt.UTC()),
	); err != nil {
		return fmt.Errorf("activate shadow review configuration approval %q: %w", repo, err)
	}
	return nil
}

type shadowReviewConfigurationApprovalRow struct {
	approvalDigest      domain.Digest
	repo                string
	repositoryID        int64
	source              domain.ShadowReviewSource
	configurationDigest domain.Digest
	recordedAt          string
	body                []byte
}

func scanShadowReviewConfigurationApprovalRow(
	sc scanner,
) (shadowReviewConfigurationApprovalRow, error) {
	var row shadowReviewConfigurationApprovalRow
	if err := sc.Scan(
		&row.approvalDigest, &row.repo, &row.repositoryID, &row.source,
		&row.configurationDigest, &row.recordedAt, &row.body,
	); err != nil {
		return shadowReviewConfigurationApprovalRow{}, err
	}
	return row, nil
}

func reconstructShadowReviewConfigurationApproval(
	row shadowReviewConfigurationApprovalRow,
) (ShadowReviewConfigurationApprovalRecord, error) {
	approval, err := decode[domain.ShadowReviewConfigurationApproval](row.body)
	if err != nil {
		return ShadowReviewConfigurationApprovalRecord{}, err
	}
	if approval.ApprovalDigest != row.approvalDigest ||
		approval.Repo != row.repo ||
		approval.RepositoryID != row.repositoryID ||
		approval.Source != row.source ||
		approval.ConfigurationDigest != row.configurationDigest {
		return ShadowReviewConfigurationApprovalRecord{}, errRowInconsistent
	}
	recordedAt, err := parseTime(row.recordedAt)
	if err != nil {
		return ShadowReviewConfigurationApprovalRecord{},
			fmt.Errorf("stored recorded_at invalid: %w", err)
	}
	return ShadowReviewConfigurationApprovalRecord{
		Approval: approval, RecordedAt: recordedAt,
	}, nil
}

// GetShadowReviewConfigurationApproval reconstructs immutable authority by
// content address and cross-checks every copied key column.
func (tx *ReadTx) GetShadowReviewConfigurationApproval(
	ctx context.Context,
	digest domain.Digest,
) (domain.ShadowReviewConfigurationApproval, error) {
	row, err := scanShadowReviewConfigurationApprovalRow(
		tx.tx.QueryRowContext(ctx, getShadowReviewConfigurationApprovalSQL, digest),
	)
	if err != nil {
		return domain.ShadowReviewConfigurationApproval{}, fmt.Errorf(
			"get shadow review configuration approval %q: %w", digest, notFoundOr(err))
	}
	record, err := reconstructShadowReviewConfigurationApproval(row)
	if err != nil {
		return domain.ShadowReviewConfigurationApproval{}, fmt.Errorf(
			"get shadow review configuration approval %q: %w", digest, err)
	}
	return record.Approval, nil
}

// InspectCurrentShadowReviewConfigurationApproval returns the current
// selection for one exact repo/source without turning a corrupt selection into
// absence. Missing authority is a deterministic inspection result.
func (tx *ReadTx) InspectCurrentShadowReviewConfigurationApproval(
	ctx context.Context,
	repo string,
	source domain.ShadowReviewSource,
) (CurrentShadowReviewConfigurationApprovalInspection, error) {
	inspection := CurrentShadowReviewConfigurationApprovalInspection{
		Repo: repo, Source: source,
	}
	var (
		activationRepo                string
		activationRepositoryID        int64
		activationSource              domain.ShadowReviewSource
		activationDigest              domain.Digest
		activationConfigurationDigest domain.Digest
		approvalDigest                sql.NullString
		approvalRepo                  sql.NullString
		approvalRepositoryID          sql.NullInt64
		approvalSource                sql.NullString
		approvalConfigurationDigest   sql.NullString
		recordedAt                    sql.NullString
		body                          []byte
	)
	err := tx.tx.QueryRowContext(
		ctx, currentShadowReviewConfigurationApprovalSQL, repo, source,
	).Scan(
		&activationRepo, &activationRepositoryID, &activationSource,
		&activationDigest, &activationConfigurationDigest,
		&approvalDigest, &approvalRepo, &approvalRepositoryID, &approvalSource,
		&approvalConfigurationDigest, &recordedAt, &body,
	)
	if errors.Is(err, sql.ErrNoRows) {
		inspection.ReconstructionError = ErrNotFound
		return inspection, nil
	}
	if err != nil {
		return inspection, fmt.Errorf(
			"inspect current shadow review configuration approval %q/%s: %w",
			repo, source, err)
	}
	inspection.Repo = activationRepo
	inspection.RepositoryID = activationRepositoryID
	inspection.Source = activationSource
	if activationRepo != repo || activationSource != source ||
		!approvalDigest.Valid || !approvalRepo.Valid ||
		!approvalRepositoryID.Valid || !approvalSource.Valid ||
		!approvalConfigurationDigest.Valid || !recordedAt.Valid || body == nil {
		inspection.ReconstructionError = errRowInconsistent
		return inspection, nil
	}
	row := shadowReviewConfigurationApprovalRow{
		approvalDigest:      domain.Digest(approvalDigest.String),
		repo:                approvalRepo.String,
		repositoryID:        approvalRepositoryID.Int64,
		source:              domain.ShadowReviewSource(approvalSource.String),
		configurationDigest: domain.Digest(approvalConfigurationDigest.String),
		recordedAt:          recordedAt.String,
		body:                body,
	}
	if activationDigest != row.approvalDigest ||
		activationRepo != row.repo ||
		activationRepositoryID != row.repositoryID ||
		activationSource != row.source ||
		activationConfigurationDigest != row.configurationDigest {
		inspection.ReconstructionError = errRowInconsistent
		return inspection, nil
	}
	record, reconstructionErr := reconstructShadowReviewConfigurationApproval(row)
	inspection.Approval = record.Approval
	inspection.ReconstructionError = reconstructionErr
	return inspection, nil
}

// RequireShadowReviewConfigurationApproved proves that every repository with
// a current trust profile separately approves the effective shadow source and
// exact configuration digest. Routed Review.ConfigDigest is never consulted.
func (tx *ReadTx) RequireShadowReviewConfigurationApproved(
	ctx context.Context,
	source domain.ShadowReviewSource,
	effective domain.Digest,
) error {
	if !slices.Contains(domain.AllShadowReviewSources, source) {
		return fmt.Errorf("invalid effective shadow review source %q: %w", source,
			errors.Join(
				domain.ErrShadowReviewConfigUnapproved,
				domain.ErrInvalidShadowReviewSource,
			))
	}
	if !contentaddr.Valid(string(effective)) {
		return fmt.Errorf("invalid effective shadow review configuration %q: %w",
			effective, domain.ErrShadowReviewConfigUnapproved)
	}
	profiles, err := tx.InspectLatestTrustProfiles(ctx)
	if err != nil {
		return err
	}
	for _, profileInspection := range profiles {
		if profileInspection.ReconstructionError != nil {
			return fmt.Errorf("current trust profile for %q is unreadable: %w",
				profileInspection.Repo,
				errors.Join(
					domain.ErrShadowReviewConfigUnapproved,
					profileInspection.ReconstructionError,
				))
		}
		profile := profileInspection.Profile
		approvalInspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(
			ctx, profile.Repo, source,
		)
		if err != nil {
			return err
		}
		if approvalInspection.ReconstructionError != nil {
			return fmt.Errorf("current shadow review configuration approval for %q/%s is unreadable: %w",
				profile.Repo, source,
				errors.Join(
					domain.ErrShadowReviewConfigUnapproved,
					approvalInspection.ReconstructionError,
				))
		}
		approval := approvalInspection.Approval
		if approval.Repo != profile.Repo ||
			approval.RepositoryID != profile.RepositoryID ||
			approval.Source != source ||
			approval.ConfigurationDigest != effective {
			return fmt.Errorf(
				"current shadow review configuration approval for %q/%s binds repository %d and config %s; current profile is repository %d and daemon effective is %s: %w",
				profile.Repo, source, approval.RepositoryID,
				approval.ConfigurationDigest, profile.RepositoryID, effective,
				domain.ErrShadowReviewConfigUnapproved,
			)
		}
	}
	return nil
}
