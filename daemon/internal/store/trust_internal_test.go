package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestTrustRowsTamperedBodyFailsClosed is the #52 re-gate for the trust
// shapes at the persistence boundary: a row whose body was altered around
// the store (raw SQL past the Record boundary) is rejected on read, because
// decode re-runs Validate and the domain shapes recompute their own digest,
// id, and trust bit. Internal test: writing the tampered row requires raw
// SQL.
func TestTrustRowsTamperedBodyFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/candidate-repo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	auth, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: profile.Repo, BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe-approved",
		VerificationOutcome:      domain.VerificationFailed,
		TrustProfileDigest:       profile.ProfileDigest,
		InvocationID:             "inv-1",
		CreatedAt:                time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}

	profileBody, err := encode(profile)
	if err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	authBody, err := encode(auth)
	if err != nil {
		t.Fatalf("encode authorization: %v", err)
	}

	// A profile body whose posture was loosened under the stored (bound)
	// digest: the §5.5 drift the digest binding exists to catch.
	tamperedProfile := strings.Replace(profileBody, `"allow_self_hosted_ci":false`, `"allow_self_hosted_ci":true`, 1)
	if tamperedProfile == profileBody {
		t.Fatal("profile tamper did not apply")
	}
	// An authorization body whose computed trust bit was flipped: a failed
	// verification claiming to authorize publication (the forged bit #168's
	// gate must never trust).
	tamperedAuth := strings.Replace(authBody, `"authorizes_publication":false`, `"authorizes_publication":true`, 1)
	if tamperedAuth == authBody {
		t.Fatal("authorization tamper did not apply")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
		profile.ProfileDigest, profile.Repo, formatTime(auth.CreatedAt), tamperedProfile); err != nil {
		t.Fatalf("insert tampered profile: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO candidate_authorizations (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		auth.ID, auth.Repo, auth.BaseSHA, auth.HeadSHA, auth.TrustProfileDigest,
		formatTime(auth.CreatedAt), tamperedAuth); err != nil {
		t.Fatalf("insert tampered authorization: %v", err)
	}

	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetTrustProfile(ctx, profile.ProfileDigest)
		return err
	})
	if !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("tampered profile read error = %v, want ErrProfileDigestMismatch", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListTrustProfiles(ctx, profile.Repo)
		return err
	})
	if !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("tampered profile list error = %v, want ErrProfileDigestMismatch", err)
	}

	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetCandidateAuthorization(ctx, auth.ID)
		return err
	})
	if !errors.Is(err, domain.ErrAuthorizationInconsistent) {
		t.Fatalf("tampered authorization read error = %v, want ErrAuthorizationInconsistent", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListCandidateAuthorizations(ctx, auth.Repo, auth.HeadSHA)
		return err
	})
	if !errors.Is(err, domain.ErrAuthorizationInconsistent) {
		t.Fatalf("tampered authorization list error = %v, want ErrAuthorizationInconsistent", err)
	}
}

// TestTrustRowsInconsistentColumnsFailClosed: a row whose extracted key
// columns disagree with a valid body is corrupt, not trusted data — the
// scanner cross-check rejects it even though the body itself validates.
func TestTrustRowsInconsistentColumnsFailClosed(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/candidate-repo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	body, err := encode(profile)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// The repo column claims a different repository than the (valid) body.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
		profile.ProfileDigest, "freeside-ai/other-repo",
		formatTime(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)), body); err != nil {
		t.Fatalf("insert inconsistent profile: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetTrustProfile(ctx, profile.ProfileDigest)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("inconsistent profile read error = %v, want errRowInconsistent", err)
	}

	audit := domain.WorkflowAudit{
		Repo:                "freeside-ai/candidate-repo",
		AuditedCommitSHA:    "cafebabe",
		AuditedAt:           time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: "sha256:workflow-audit",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	auditBody, err := encode(audit)
	if err != nil {
		t.Fatalf("encode audit: %v", err)
	}
	// The digest column disagrees with the body's attested digest.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_audits (repo, audited_commit_sha, audited_at, workflow_audit_digest, body) VALUES (?, ?, ?, ?, ?)`,
		audit.Repo, audit.AuditedCommitSHA, formatTime(audit.AuditedAt),
		"sha256:other", auditBody); err != nil {
		t.Fatalf("insert inconsistent audit: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListWorkflowAudits(ctx, audit.Repo)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("inconsistent audit list error = %v, want errRowInconsistent", err)
	}
}
