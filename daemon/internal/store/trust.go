package store

import (
	"context"
	"fmt"
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
	ID    int64
	Audit domain.WorkflowAudit
}

const (
	recordTrustProfileSQL = `
INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body)
VALUES (?, ?, ?, ?)
ON CONFLICT (profile_digest) DO NOTHING`
	getTrustProfileSQL = `SELECT repo, recorded_at, body FROM trust_profiles WHERE profile_digest = ?`
	// Lists order by rowid (insertion order), never by the RFC3339Nano text
	// columns: trailing zeros are trimmed, so sub-second instants misorder
	// lexicographically ("...05Z" sorts after "...05.5Z"), and the profile
	// list is what a consumer selecting the current binding orders by.
	listTrustProfilesSQL = `
SELECT profile_digest, repo, recorded_at, body FROM trust_profiles
WHERE repo = ? ORDER BY rowid`

	recordWorkflowAuditSQL = `
INSERT INTO workflow_audits (repo, audited_commit_sha, audited_at, workflow_audit_digest, body)
VALUES (?, ?, ?, ?, ?)`
	listWorkflowAuditsSQL = `
SELECT id, repo, audited_commit_sha, workflow_audit_digest, body
FROM workflow_audits WHERE repo = ? ORDER BY id`

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

// RecordTrustProfile persists one human-approved profile revision,
// write-once per content digest: a byte-identical replay converges on the
// existing row, a different profile under the same digest is an
// ErrImmutableConflict (and unreachable without a digest collision, since
// encode's Validate recomputes the digest from the content).
func (tx *InternalTx) RecordTrustProfile(ctx context.Context, profile domain.AutomationTrustProfile, recordedAt time.Time) error {
	body, err := encode(profile)
	if err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
	}
	if recordedAt.IsZero() {
		return fmt.Errorf("record trust profile %q: zero recorded_at", profile.Repo)
	}
	if err := tx.putImmutable(ctx, recordTrustProfileSQL,
		[]any{profile.ProfileDigest, profile.Repo, formatTime(recordedAt.UTC()), body},
		`SELECT body FROM trust_profiles WHERE profile_digest = ?`,
		[]any{profile.ProfileDigest}, body); err != nil {
		return fmt.Errorf("record trust profile %q: %w", profile.Repo, err)
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
	at, err := time.Parse(time.RFC3339Nano, recordedAt)
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

// ListTrustProfiles returns every recorded profile revision for a
// repository in recorded order, for the consumer that selects the current
// binding.
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
	body, err := encode(audit)
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	res, err := tx.tx.ExecContext(ctx, recordWorkflowAuditSQL,
		audit.Repo, audit.AuditedCommitSHA, formatTime(audit.AuditedAt.UTC()),
		audit.WorkflowAuditDigest, body)
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return WorkflowAuditRecord{}, fmt.Errorf("record workflow audit %q: %w", audit.Repo, err)
	}
	return WorkflowAuditRecord{ID: id, Audit: audit}, nil
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
