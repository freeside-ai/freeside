package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Trust bookkeeping (plan §5.5, §5.6; issue #172): automation trust
// profiles, workflow audits, and candidate authorizations. All three are
// daemon-internal like the mint audit — never synchronized, so writes live
// on InternalTx with non-Put names (the #38 invariant) and rows carry no
// entity_version/as_of_revision.
//
// The re-gate at these boundaries needs no store-side policy parameter:
// the domain shapes are self-certifying (Validate recomputes the profile
// digest, the authorization id, and authorizes_publication from the body's
// own bound facts), so encode rejects a forged write and decode rejects a
// tampered row. Reads additionally cross-check the extracted key columns
// against the decoded body per the scanner convention.

// TrustProfileRecord pairs a stored profile with the bookkeeping instant it
// was recorded. recorded_at is not profile content (the digest does not
// cover it): it exists so a consumer selecting the current profile can
// order revisions.
type TrustProfileRecord struct {
	Profile    domain.AutomationTrustProfile
	RecordedAt time.Time
}

// WorkflowAuditRecord pairs a stored audit observation with its assigned
// insertion id.
type WorkflowAuditRecord struct {
	ID    int64                `json:"id"`
	Audit domain.WorkflowAudit `json:"audit"`
}

// WorkflowAuditReview is the daemon-internal projection consumed by the
// one-time profile review and later drift review. Approved is selected by the
// active profile's bound audit digest; Observed is the latest audit row.
// ChangedFields names the deterministic top-level evidence sections that
// differ, while each side carries the exact digest-bound body.
type WorkflowAuditReview struct {
	Profile       domain.AutomationTrustProfile `json:"profile"`
	Approved      WorkflowAuditReviewSide       `json:"approved"`
	Observed      WorkflowAuditReviewSide       `json:"observed"`
	ChangedFields []string                      `json:"changed_fields"`
}

type WorkflowAuditReviewSide struct {
	Audit    WorkflowAuditRecord          `json:"audit"`
	Evidence domain.WorkflowAuditEvidence `json:"evidence"`
}

const (
	recordTrustProfileSQL = `
INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body)
VALUES (?, ?, ?, ?)
ON CONFLICT (profile_digest) DO NOTHING`
	recordTrustProfileActivationSQL = `
INSERT INTO trust_profile_activations (
    repo, profile_digest, workflow_audit_digest, activated_at
)
VALUES (?, ?, ?, ?)`
	getTrustProfileSQL = `SELECT repo, recorded_at, body FROM trust_profiles WHERE profile_digest = ?`
	// Lists order by rowid (insertion order), never by the RFC3339Nano text
	// columns: trailing zeros are trimmed, so sub-second instants misorder
	// lexicographically ("...05Z" sorts after "...05.5Z"), and the profile
	// list is what a consumer selecting the current binding orders by.
	listTrustProfilesSQL = `
SELECT profile_digest, repo, recorded_at, body FROM trust_profiles
WHERE repo = ? ORDER BY rowid`
	latestTrustProfileSQL = `
SELECT p.profile_digest, p.repo, p.recorded_at, p.body
FROM trust_profile_activations AS a
JOIN trust_profiles AS p
  ON p.repo = a.repo AND p.profile_digest = a.profile_digest
WHERE a.repo = ? ORDER BY a.id DESC LIMIT 1`
	latestActiveWorkflowAuditBindingSQL = `
SELECT a.profile_digest, a.workflow_audit_digest, p.repo, p.body
FROM trust_profile_activations AS a
JOIN trust_profiles AS p
  ON p.repo = a.repo AND p.profile_digest = a.profile_digest
WHERE a.repo = ? ORDER BY a.id DESC LIMIT 1`

	recordWorkflowAuditSQL = `
INSERT INTO workflow_audits (repo, audited_commit_sha, audited_at, workflow_audit_digest, body)
VALUES (?, ?, ?, ?, ?)`
	listWorkflowAuditsSQL = `
SELECT id, repo, audited_commit_sha, workflow_audit_digest, body
FROM workflow_audits WHERE repo = ? ORDER BY id`
	recordWorkflowAuditEvidenceSQL = `
INSERT INTO workflow_audit_evidence (repo, workflow_audit_digest, body)
VALUES (?, ?, ?)
ON CONFLICT (repo, workflow_audit_digest) DO NOTHING`
	getWorkflowAuditEvidenceSQL = `
SELECT body FROM workflow_audit_evidence
WHERE repo = ? AND workflow_audit_digest = ?`
	pruneWorkflowAuditEvidenceSQL = `
DELETE FROM workflow_audit_evidence
WHERE repo = ? AND workflow_audit_digest <> ? AND workflow_audit_digest <> ?`
	deleteWorkflowAuditEvidenceSQL = `
DELETE FROM workflow_audit_evidence WHERE repo = ?`
	latestWorkflowAuditSQL = `
SELECT id, repo, audited_commit_sha, workflow_audit_digest, body
FROM workflow_audits WHERE repo = ? ORDER BY id DESC LIMIT 1`
	latestWorkflowAuditForDigestSQL = `
SELECT id, repo, audited_commit_sha, workflow_audit_digest, body
FROM workflow_audits
WHERE repo = ? AND workflow_audit_digest = ? ORDER BY id DESC LIMIT 1`
	recordAuthorizationSQL = `
INSERT INTO candidate_authorizations (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`
	getAuthorizationSQL = `
SELECT repo, base_sha, head_sha, trust_profile_digest, body
FROM candidate_authorizations WHERE id = ?`
	listAuthorizationsSQL = `
SELECT id, repo, base_sha, head_sha, trust_profile_digest, body
FROM candidate_authorizations WHERE repo = ? AND head_sha = ?
ORDER BY rowid`
)

// RecordTrustProfile persists and activates one new human-approved profile
// revision, write-once per content digest. A byte-identical replay converges
// without recording another activation: otherwise a stale retry of profile A
// after profile B could silently reactivate A. ActivateTrustProfile is the
// explicit A -> B -> A owner-decision path.
func (tx *InternalTx) RecordTrustProfile(ctx context.Context, profile domain.AutomationTrustProfile, recordedAt time.Time) error {
	body, err := encode(profile)
	if err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
	}
	if recordedAt.IsZero() {
		return fmt.Errorf("record trust profile %q: zero recorded_at", profile.Repo)
	}
	res, err := tx.tx.ExecContext(ctx, recordTrustProfileSQL,
		profile.ProfileDigest, profile.Repo, formatTime(recordedAt.UTC()), body)
	if err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
	}
	if inserted == 0 {
		var existing string
		if err := tx.tx.QueryRowContext(ctx,
			`SELECT body FROM trust_profiles WHERE profile_digest = ?`,
			profile.ProfileDigest).Scan(&existing); err != nil {
			return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
		}
		if existing != body {
			return fmt.Errorf("record trust profile %q: %w", profile.Repo, ErrImmutableConflict)
		}
		return nil
	}
	if err := tx.recordTrustProfileActivation(ctx, profile, recordedAt); err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
	}
	return nil
}

// RecordInactiveTrustProfile persists one reviewed profile revision without
// making it current. Operational onboarding uses this as the recoverable first
// phase before it promotes an installation binding; ActivateTrustProfile is
// the explicit final commit. A byte-identical replay converges.
func (tx *InternalTx) RecordInactiveTrustProfile(
	ctx context.Context,
	profile domain.AutomationTrustProfile,
	recordedAt time.Time,
) error {
	body, err := encode(profile)
	if err != nil {
		return fmt.Errorf("record inactive trust profile %q: %w", profile.Repo, err)
	}
	if recordedAt.IsZero() {
		return fmt.Errorf("record inactive trust profile %q: zero recorded_at", profile.Repo)
	}
	res, err := tx.tx.ExecContext(ctx, recordTrustProfileSQL,
		profile.ProfileDigest, profile.Repo, formatTime(recordedAt.UTC()), body)
	if err != nil {
		return fmt.Errorf("record inactive trust profile %q: %w", profile.Repo, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record inactive trust profile %q: %w", profile.Repo, err)
	}
	if inserted != 0 {
		return nil
	}
	var existing string
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM trust_profiles WHERE profile_digest = ?`,
		profile.ProfileDigest).Scan(&existing); err != nil {
		return fmt.Errorf("record inactive trust profile %q: %w", profile.Repo, err)
	}
	if existing != body {
		return fmt.Errorf("record inactive trust profile %q: %w",
			profile.Repo, ErrImmutableConflict)
	}
	return nil
}

// ActivateTrustProfile explicitly selects a previously recorded profile as
// current. It validates the addressed immutable row before appending the
// decision, so a stale-encoding or cross-repository digest cannot become
// current. Repeating an activation is harmless history and leaves the same
// profile current; callers that need command-level idempotency supply it at
// the command/inbox boundary.
func (tx *InternalTx) ActivateTrustProfile(ctx context.Context, repo string, digest domain.Digest, activatedAt time.Time) error {
	if repo == "" || digest == "" {
		return fmt.Errorf("activate trust profile: empty repo or digest")
	}
	if activatedAt.IsZero() {
		return fmt.Errorf("activate trust profile %q: zero activated_at", repo)
	}
	profile, err := tx.GetTrustProfile(ctx, digest)
	if err != nil {
		return fmt.Errorf("activate trust profile %q: %w", repo, err)
	}
	if profile.Repo != repo {
		return fmt.Errorf("activate trust profile %q: digest belongs to %q", repo, profile.Repo)
	}
	if err := tx.recordTrustProfileActivation(ctx, profile, activatedAt); err != nil {
		return fmt.Errorf("activate trust profile %q: %w", repo, err)
	}
	return nil
}

func (tx *InternalTx) recordTrustProfileActivation(
	ctx context.Context, profile domain.AutomationTrustProfile, activatedAt time.Time,
) error {
	if err := tx.requireRetainedProfileEvidence(ctx, profile); err != nil {
		return err
	}
	_, err := tx.tx.ExecContext(ctx, recordTrustProfileActivationSQL,
		profile.Repo, profile.ProfileDigest, profile.WorkflowAuditDigest,
		formatTime(activatedAt.UTC()))
	if err != nil {
		return err
	}
	return tx.pruneWorkflowAuditEvidence(ctx, profile.Repo, profile.WorkflowAuditDigest)
}

// requireRetainedProfileEvidence preserves compatibility for legacy profiles
// that never had an audit observation, while preventing activation of a
// digest whose once-retained provenance has already been pruned or deleted.
func (tx *InternalTx) requireRetainedProfileEvidence(
	ctx context.Context, profile domain.AutomationTrustProfile,
) error {
	audits, err := tx.ListWorkflowAudits(ctx, profile.Repo)
	if err != nil {
		return fmt.Errorf("check workflow audit evidence for profile %q: %w", profile.Repo, err)
	}
	hasAudit := false
	for _, rec := range audits {
		if rec.Audit.WorkflowAuditDigest == profile.WorkflowAuditDigest {
			hasAudit = true
			break
		}
	}
	if !hasAudit {
		return nil
	}
	if _, err := tx.workflowAuditEvidence(ctx, profile.Repo, profile.WorkflowAuditDigest); err != nil {
		return fmt.Errorf("load workflow audit evidence for profile %q: %w", profile.Repo, err)
	}
	return nil
}

// scanTrustProfile is the one reconstruction path for profile rows: decode
// re-runs Validate (which recomputes the content digest), and the extracted
// key columns are cross-checked against the body so a row edited around the
// store fails closed.
func scanTrustProfile(sc scanner, wantDigest domain.Digest) (TrustProfileRecord, error) {
	var (
		digest     = wantDigest
		repo       string
		recordedAt string
		body       []byte
	)
	dest := []any{&repo, &recordedAt, &body}
	if wantDigest == "" {
		dest = append([]any{&digest}, dest...)
	}
	if err := sc.Scan(dest...); err != nil {
		return TrustProfileRecord{}, err
	}
	profile, err := decode[domain.AutomationTrustProfile](body)
	if err != nil {
		return TrustProfileRecord{}, err
	}
	if profile.ProfileDigest != digest || profile.Repo != repo {
		return TrustProfileRecord{}, errRowInconsistent
	}
	at, err := parseTime(recordedAt)
	if err != nil {
		return TrustProfileRecord{}, fmt.Errorf("stored recorded_at invalid: %w", err)
	}
	return TrustProfileRecord{Profile: profile, RecordedAt: at}, nil
}

// GetTrustProfile reconstructs one profile by its content digest.
func (tx *ReadTx) GetTrustProfile(ctx context.Context, digest domain.Digest) (domain.AutomationTrustProfile, error) {
	row := tx.tx.QueryRowContext(ctx, getTrustProfileSQL, digest)
	rec, err := scanTrustProfile(row, digest)
	if err != nil {
		return domain.AutomationTrustProfile{}, fmt.Errorf("get trust profile %q: %w", digest, notFoundOr(err))
	}
	return rec.Profile, nil
}

// LatestTrustProfile reconstructs the explicitly activated profile for a
// repository: the current-binding read (plan §5.5). It
// deliberately validates no older history: after an encoding-version bump
// every pre-bump row is permanently stale by design, so a full-history read
// would fail forever and make the documented re-approval recovery
// unreachable (#222 review). The selected row itself still fails closed while
// it is stale (the re-approval not yet recorded), and a stale historical
// digest addressed directly (GetTrustProfile, an old authorization's
// binding) stays fail-closed.
func (tx *ReadTx) LatestTrustProfile(ctx context.Context, repo string) (domain.AutomationTrustProfile, error) {
	row := tx.tx.QueryRowContext(ctx, latestTrustProfileSQL, repo)
	rec, err := scanTrustProfile(row, "")
	if err != nil {
		return domain.AutomationTrustProfile{}, fmt.Errorf("latest trust profile %q: %w", repo, notFoundOr(err))
	}
	return rec.Profile, nil
}

// ListTrustProfiles returns every recorded profile revision for a
// repository in recorded order, validating every row: the audit/history
// read. A consumer selecting the current binding uses LatestTrustProfile,
// which stale history cannot poison.
func (tx *ReadTx) ListTrustProfiles(ctx context.Context, repo string) ([]TrustProfileRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, listTrustProfilesSQL, repo)
	if err != nil {
		return nil, fmt.Errorf("list trust profiles %q: %w", repo, err)
	}
	defer func() { _ = rows.Close() }()
	var recs []TrustProfileRecord
	for rows.Next() {
		rec, err := scanTrustProfile(rows, "")
		if err != nil {
			return nil, fmt.Errorf("list trust profiles %q: %w", repo, err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trust profiles %q: %w", repo, err)
	}
	return recs, nil
}

// RecordWorkflowAudit appends one audit observation and returns it with its
// assigned id. Insert-only, no idempotency key: two identical audits are
// two real observations (the mint-audit shape).
func (tx *InternalTx) RecordWorkflowAudit(ctx context.Context, audit domain.WorkflowAudit) (WorkflowAuditRecord, error) {
	if audit.Evidence == nil {
		return WorkflowAuditRecord{}, fmt.Errorf(
			"record workflow audit %q: %w", audit.Repo, domain.ErrWorkflowAuditEvidenceInvalid,
		)
	}
	body, err := encode(audit)
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	evidenceBody := audit.Evidence.Bytes()
	res, err := tx.tx.ExecContext(
		ctx, recordWorkflowAuditEvidenceSQL, audit.Repo, audit.WorkflowAuditDigest, evidenceBody,
	)
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit evidence %q: %w", audit.Repo, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit evidence %q: %w", audit.Repo, err)
	}
	if inserted == 0 {
		var existing []byte
		if err := tx.tx.QueryRowContext(
			ctx, getWorkflowAuditEvidenceSQL, audit.Repo, audit.WorkflowAuditDigest,
		).Scan(&existing); err != nil {
			return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit evidence %q: %w", audit.Repo, err)
		}
		if !bytes.Equal(existing, evidenceBody) {
			return WorkflowAuditRecord{}, fmt.Errorf(
				"record workflow audit evidence %q: %w", audit.Repo, ErrImmutableConflict,
			)
		}
	}
	res, err = tx.tx.ExecContext(ctx, recordWorkflowAuditSQL,
		audit.Repo, audit.AuditedCommitSHA, formatTime(audit.AuditedAt.UTC()),
		audit.WorkflowAuditDigest, body)
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	if err := tx.pruneWorkflowAuditEvidence(ctx, audit.Repo, ""); err != nil {
		return WorkflowAuditRecord{}, err
	}
	return WorkflowAuditRecord{ID: id, Audit: audit}, nil
}

func (tx *InternalTx) pruneWorkflowAuditEvidence(
	ctx context.Context, repo string, approved domain.Digest,
) error {
	if approved == "" {
		var authenticated bool
		var err error
		approved, authenticated, err = tx.authenticatedActiveWorkflowAuditDigest(ctx, repo)
		if err != nil {
			return err
		}
		if !authenticated {
			// A missing, stale, or tampered binding is not deletion
			// authority. Retain extra bodies until a validated activation
			// re-establishes the bounded approved/observed set.
			return nil
		}
	}
	observed := domain.Digest("")
	latest, err := tx.scanWorkflowAuditRecord(ctx, latestWorkflowAuditSQL, repo)
	switch {
	case err == nil:
		observed = latest.Audit.WorkflowAuditDigest
	case errors.Is(err, ErrNotFound):
	default:
		return fmt.Errorf("prune workflow audit evidence %q latest audit: %w", repo, err)
	}
	if _, err := tx.tx.ExecContext(ctx, pruneWorkflowAuditEvidenceSQL, repo, observed, approved); err != nil {
		return fmt.Errorf("prune workflow audit evidence %q: %w", repo, err)
	}
	return nil
}

func (tx *InternalTx) authenticatedActiveWorkflowAuditDigest(
	ctx context.Context, repo string,
) (domain.Digest, bool, error) {
	var (
		profileDigest domain.Digest
		auditDigest   domain.Digest
		storedRepo    string
		body          []byte
	)
	err := tx.tx.QueryRowContext(ctx, latestActiveWorkflowAuditBindingSQL, repo).Scan(
		&profileDigest, &auditDigest, &storedRepo, &body,
	)
	switch {
	case err == nil:
	case errors.Is(notFoundOr(err), ErrNotFound):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("prune workflow audit evidence %q active profile: %w", repo, err)
	}
	profile, err := decode[domain.AutomationTrustProfile](body)
	if err != nil {
		return "", false, nil
	}
	if storedRepo != repo ||
		profile.Repo != repo ||
		profile.ProfileDigest != profileDigest ||
		profile.WorkflowAuditDigest != auditDigest {
		return "", false, nil
	}
	return auditDigest, true, nil
}

// DeleteWorkflowAuditEvidence removes every retained evidence body for a
// repository without rewriting its audit/profile history. A later review
// reports the missing body honestly until a fresh audit reintroduces it.
func (tx *InternalTx) DeleteWorkflowAuditEvidence(ctx context.Context, repo string) error {
	if repo == "" {
		return fmt.Errorf("delete workflow audit evidence: empty repo")
	}
	if _, err := tx.tx.ExecContext(ctx, deleteWorkflowAuditEvidenceSQL, repo); err != nil {
		return fmt.Errorf("delete workflow audit evidence %q: %w", repo, err)
	}
	return nil
}

// ListWorkflowAudits returns every audit observation for a repository in
// insertion order, for the drift comparison at the publication decision
// point and for tests.
func (tx *ReadTx) ListWorkflowAudits(ctx context.Context, repo string) ([]WorkflowAuditRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, listWorkflowAuditsSQL, repo)
	if err != nil {
		return nil, fmt.Errorf("list workflow audits %q: %w", repo, err)
	}
	defer func() { _ = rows.Close() }()
	var recs []WorkflowAuditRecord
	for rows.Next() {
		var (
			rec         WorkflowAuditRecord
			rowRepo     string
			commitSHA   string
			auditDigest string
			body        []byte
		)
		if err := rows.Scan(&rec.ID, &rowRepo, &commitSHA, &auditDigest, &body); err != nil {
			return nil, fmt.Errorf("list workflow audits %q: %w", repo, err)
		}
		audit, err := decode[domain.WorkflowAudit](body)
		if err != nil {
			return nil, fmt.Errorf("list workflow audits %q: %w", repo, err)
		}
		if audit.Repo != rowRepo || audit.AuditedCommitSHA != commitSHA ||
			audit.WorkflowAuditDigest != domain.Digest(auditDigest) {
			return nil, fmt.Errorf("list workflow audits %q: %w", repo, errRowInconsistent)
		}
		rec.Audit = audit
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow audits %q: %w", repo, err)
	}
	return recs, nil
}

// WorkflowAuditReview reconstructs the active approved and latest observed
// digest-bound evidence. It is intentionally daemon-internal: evidence may
// contain complete workflow files and is not part of sync or general audit
// listing. Missing retained evidence fails honestly with ErrNotFound.
func (tx *ReadTx) WorkflowAuditReview(ctx context.Context, repo string) (WorkflowAuditReview, error) {
	profile, err := tx.LatestTrustProfile(ctx, repo)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q: %w", repo, err)
	}
	return tx.workflowAuditReviewForProfile(ctx, profile)
}

// WorkflowAuditReviewForProfile builds the same projection for a proposed
// profile before it is activated. This is the one-time onboarding review
// seam: the proposed digest must already name a retained audit observation,
// but approval is not required to inspect what that digest binds.
func (tx *ReadTx) WorkflowAuditReviewForProfile(
	ctx context.Context, profile domain.AutomationTrustProfile,
) (WorkflowAuditReview, error) {
	if err := profile.Validate(); err != nil {
		return WorkflowAuditReview{}, fmt.Errorf(
			"workflow audit review proposed profile %q: %w", profile.Repo, err,
		)
	}
	return tx.workflowAuditReviewForProfile(ctx, profile)
}

func (tx *ReadTx) workflowAuditReviewForProfile(
	ctx context.Context, profile domain.AutomationTrustProfile,
) (WorkflowAuditReview, error) {
	repo := profile.Repo
	approved, err := tx.scanWorkflowAuditRecord(
		ctx, latestWorkflowAuditForDigestSQL, repo, profile.WorkflowAuditDigest,
	)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q approved audit: %w", repo, err)
	}
	observed, err := tx.scanWorkflowAuditRecord(ctx, latestWorkflowAuditSQL, repo)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q observed audit: %w", repo, err)
	}
	approvedEvidence, err := tx.workflowAuditEvidence(ctx, repo, profile.WorkflowAuditDigest)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q approved evidence: %w", repo, err)
	}
	observedEvidence, err := tx.workflowAuditEvidence(ctx, repo, observed.Audit.WorkflowAuditDigest)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q observed evidence: %w", repo, err)
	}
	changed, err := workflowAuditEvidenceChangedFields(approvedEvidence, observedEvidence)
	if err != nil {
		return WorkflowAuditReview{}, fmt.Errorf("workflow audit review %q compare evidence: %w", repo, err)
	}
	return WorkflowAuditReview{
		Profile: profile,
		Approved: WorkflowAuditReviewSide{
			Audit: approved, Evidence: approvedEvidence,
		},
		Observed: WorkflowAuditReviewSide{
			Audit: observed, Evidence: observedEvidence,
		},
		ChangedFields: changed,
	}, nil
}

func (tx *ReadTx) scanWorkflowAuditRecord(
	ctx context.Context, query string, args ...any,
) (WorkflowAuditRecord, error) {
	var (
		rec         WorkflowAuditRecord
		rowRepo     string
		commitSHA   string
		auditDigest string
		body        []byte
	)
	if err := tx.tx.QueryRowContext(ctx, query, args...).Scan(
		&rec.ID, &rowRepo, &commitSHA, &auditDigest, &body,
	); err != nil {
		return WorkflowAuditRecord{}, notFoundOr(err)
	}
	audit, err := decode[domain.WorkflowAudit](body)
	if err != nil {
		return WorkflowAuditRecord{}, err
	}
	if audit.Repo != rowRepo || audit.AuditedCommitSHA != commitSHA ||
		audit.WorkflowAuditDigest != domain.Digest(auditDigest) {
		return WorkflowAuditRecord{}, errRowInconsistent
	}
	rec.Audit = audit
	return rec, nil
}

func (tx *ReadTx) workflowAuditEvidence(
	ctx context.Context, repo string, digest domain.Digest,
) (domain.WorkflowAuditEvidence, error) {
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, getWorkflowAuditEvidenceSQL, repo, digest).Scan(&body); err != nil {
		return domain.WorkflowAuditEvidence{}, notFoundOr(err)
	}
	evidence, err := domain.NewWorkflowAuditEvidence(body)
	if err != nil {
		return domain.WorkflowAuditEvidence{}, err
	}
	if err := evidence.ValidateBinding(repo, digest); err != nil {
		return domain.WorkflowAuditEvidence{}, err
	}
	return evidence, nil
}

func workflowAuditEvidenceChangedFields(
	approved, observed domain.WorkflowAuditEvidence,
) ([]string, error) {
	var approvedFields, observedFields map[string]json.RawMessage
	if err := json.Unmarshal(approved.Bytes(), &approvedFields); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(observed.Bytes(), &observedFields); err != nil {
		return nil, err
	}
	fieldSet := make(map[string]struct{}, len(approvedFields)+len(observedFields))
	for field := range approvedFields {
		fieldSet[field] = struct{}{}
	}
	for field := range observedFields {
		fieldSet[field] = struct{}{}
	}
	changed := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		if !bytes.Equal(approvedFields[field], observedFields[field]) {
			changed = append(changed, field)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// RecordCandidateAuthorization persists one daemon-authored authorization,
// write-once per content id: a byte-identical replay converges, a
// same-id write with different content is an ErrImmutableConflict. The
// schema enforces the rest loudly rather than silently: an authorization
// whose bound (repo, profile digest) pair has no trust_profiles row — the
// profile was never recorded, or it belongs to a different repository —
// violates the composite foreign key (fail closed — publication trust must
// not dangle, and one repository's candidates never bind another's
// automation posture), and a second, different authorization for the same
// (repo, head, profile) violates the uniqueness key.
func (tx *InternalTx) RecordCandidateAuthorization(ctx context.Context, a domain.CandidateAuthorization) error {
	body, err := encode(a)
	if err != nil {
		return fmt.Errorf("record candidate authorization %q: %w", a.ID, err)
	}
	if err := tx.putImmutable(ctx, recordAuthorizationSQL,
		[]any{a.ID, a.Repo, a.BaseSHA, a.HeadSHA, a.TrustProfileDigest, formatTime(a.CreatedAt.UTC()), body},
		`SELECT body FROM candidate_authorizations WHERE id = ?`,
		[]any{a.ID}, body); err != nil {
		return fmt.Errorf("record candidate authorization %q: %w", a.ID, err)
	}
	return nil
}

// scanAuthorization is the one reconstruction path for authorization rows:
// decode re-runs Validate (which recomputes the id and the
// authorizes_publication bit from the bound facts), and the extracted
// binding columns are cross-checked against the body.
func scanAuthorization(sc scanner, wantID domain.Digest) (domain.CandidateAuthorization, error) {
	var (
		id            = wantID
		repo          string
		baseSHA       string
		headSHA       string
		profileDigest string
		body          []byte
	)
	dest := []any{&repo, &baseSHA, &headSHA, &profileDigest, &body}
	if wantID == "" {
		dest = append([]any{&id}, dest...)
	}
	if err := sc.Scan(dest...); err != nil {
		return domain.CandidateAuthorization{}, err
	}
	a, err := decode[domain.CandidateAuthorization](body)
	if err != nil {
		return domain.CandidateAuthorization{}, err
	}
	if a.ID != id || a.Repo != repo || a.BaseSHA != baseSHA || a.HeadSHA != headSHA ||
		a.TrustProfileDigest != domain.Digest(profileDigest) {
		return domain.CandidateAuthorization{}, errRowInconsistent
	}
	return a, nil
}

// GetCandidateAuthorization reconstructs one authorization by its content
// id.
func (tx *ReadTx) GetCandidateAuthorization(ctx context.Context, id domain.Digest) (domain.CandidateAuthorization, error) {
	row := tx.tx.QueryRowContext(ctx, getAuthorizationSQL, id)
	a, err := scanAuthorization(row, id)
	if err != nil {
		return domain.CandidateAuthorization{}, fmt.Errorf("get candidate authorization %q: %w", id, notFoundOr(err))
	}
	return a, nil
}

// ListCandidateAuthorizations returns every authorization recorded for a
// candidate head in insertion order, for the publication gate that decides
// whether a current, authorizing record exists.
func (tx *ReadTx) ListCandidateAuthorizations(ctx context.Context, repo, headSHA string) ([]domain.CandidateAuthorization, error) {
	rows, err := tx.tx.QueryContext(ctx, listAuthorizationsSQL, repo, headSHA)
	if err != nil {
		return nil, fmt.Errorf("list candidate authorizations %q %q: %w", repo, headSHA, err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.CandidateAuthorization
	for rows.Next() {
		a, err := scanAuthorization(rows, "")
		if err != nil {
			return nil, fmt.Errorf("list candidate authorizations %q %q: %w", repo, headSHA, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list candidate authorizations %q %q: %w", repo, headSHA, err)
	}
	return out, nil
}
