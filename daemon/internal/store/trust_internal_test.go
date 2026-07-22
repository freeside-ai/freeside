package store

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
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
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
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

// Captured verbatim from the v2 build: encode() of a minimal valid profile
// (no commit_plan or message_ruleset members existed). The digest is
// authentic for this content under the v2 encoding.
const (
	staleV2ProfileDigest = "sha256:2a6ed3b4091ca53f6b23a0af9153d3710a91611ee19d6ea2c21d3fe4c0a9b032"
	staleV2ProfileBody   = `{"repo":"freeside-ai/candidate-repo","pr_execution":"audited_same_repo","candidate_automation_changes":"block","pr_github_token_permissions":"read_only","allow_oidc":false,"allow_environment_secrets":false,"allow_secret_bearing_pr_jobs":false,"allow_self_hosted_ci":false,"allow_pull_request_target":false,"workflow_audit_digest":"sha256:workflow-audit","review":{"mode":"auto","config_digest":"sha256:review-config"},"protected_paths":{"extra_automation_control_patterns":null,"extra_reviewer_instruction_patterns":null,"extra_git_metadata_patterns":null,"extra_verification_control_patterns":null,"extra_prompts_and_policy_patterns":null,"extra_egress_and_trust_patterns":null,"extra_materiality_rules_patterns":null},"profile_digest":"sha256:2a6ed3b4091ca53f6b23a0af9153d3710a91611ee19d6ea2c21d3fe4c0a9b032"}`
)

// TestTrustProfileStaleEncodingRowFailsClosed is the migration-path proof
// for trust-profile encoding bumps at the persistence boundary: a row recorded under
// the v2 encoding (this literal body and digest were captured from the v2
// build, so the digest is authentic for its content under v2) fails decode's
// Validate recompute under v4 and surfaces as a hard error, never ErrNotFound
// and never a silently defaulted profile. The only path back to a readable
// profile is a human re-recording an owner-approved current profile, which is how
// the conservative single_commit default arrives (plan §5.5 drift recovery;
// the v2 precedent is the protected-path widening bump).
func TestTrustProfileStaleEncodingRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	const v2Digest = staleV2ProfileDigest
	const v2Body = staleV2ProfileBody

	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
		v2Digest, "freeside-ai/candidate-repo",
		formatTime(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)), v2Body); err != nil {
		t.Fatalf("insert v2 row: %v", err)
	}

	// decode's Validate rejects the row on its first post-v2 invariant: the
	// commit_plan member a v2 body cannot carry. The digest recompute would
	// reject it too (the version string participates in the address; the
	// domain-level re-approval test pins that class); either way the read is
	// a hard error, so no v2 row is ever silently defaulted into a current
	// profile.
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetTrustProfile(ctx, domain.Digest(v2Digest))
		return err
	})
	if !errors.Is(err, domain.ErrInvalidCommitPlanMode) {
		t.Fatalf("stale v2 row read error = %v, want ErrInvalidCommitPlanMode", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("stale v2 row surfaced as ErrNotFound; a stale profile must be a hard error, not a miss: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListTrustProfiles(ctx, "freeside-ai/candidate-repo")
		return err
	})
	if !errors.Is(err, domain.ErrInvalidCommitPlanMode) {
		t.Fatalf("stale v2 row list error = %v, want ErrInvalidCommitPlanMode", err)
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
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
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

// TestLatestTrustProfileSurvivesStaleHistory is the recovery half of the
// migration path (#222 review): once the owner records a re-approved current
// profile, the current-binding read returns it even though the stale v2 row
// remains in history; before that re-approval the newest row is the stale
// one and the read still fails closed. The validating full-history list
// keeps failing either way, so stale history is never silently readable.
func TestLatestTrustProfileSurvivesStaleHistory(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
		staleV2ProfileDigest, "freeside-ai/candidate-repo",
		formatTime(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)), staleV2ProfileBody); err != nil {
		t.Fatalf("insert v2 row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profile_activations (repo, profile_digest, activated_at) VALUES (?, ?, ?)`,
		"freeside-ai/candidate-repo", staleV2ProfileDigest,
		formatTime(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("activate v2 row: %v", err)
	}

	// Before re-approval the stale row is the newest: still fail closed.
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.LatestTrustProfile(ctx, "freeside-ai/candidate-repo")
		return err
	})
	if !errors.Is(err, domain.ErrInvalidCommitPlanMode) {
		t.Fatalf("latest over only a stale row error = %v, want ErrInvalidCommitPlanMode", err)
	}

	// The owner records the re-approved current profile: same content plus the
	// explicit policy keys, a new digest, a new row.
	reapproved, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/candidate-repo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("re-approved profile: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordTrustProfile(ctx, reapproved, time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC))
	}); err != nil {
		t.Fatalf("record re-approved profile: %v", err)
	}

	// The current-binding read recovers the moment the re-approval lands.
	var current domain.AutomationTrustProfile
	if err := s.Read(ctx, func(tx *ReadTx) error {
		p, err := tx.LatestTrustProfile(ctx, "freeside-ai/candidate-repo")
		current = p
		return err
	}); err != nil {
		t.Fatalf("latest after re-approval: %v", err)
	}
	if current.ProfileDigest != reapproved.ProfileDigest {
		t.Fatalf("latest profile digest = %q, want re-approved %q", current.ProfileDigest, reapproved.ProfileDigest)
	}

	// The validating full-history read still fails closed on the stale row.
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListTrustProfiles(ctx, "freeside-ai/candidate-repo")
		return err
	})
	if !errors.Is(err, domain.ErrInvalidCommitPlanMode) {
		t.Fatalf("history list after re-approval error = %v, want ErrInvalidCommitPlanMode", err)
	}
}

func TestTrustProfileActivationMigrationBackfillsLatestProfile(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	files, err := fs.Glob(migrations.FS, "000[1-7]_*.sql")
	if err != nil {
		t.Fatalf("glob pre-activation migrations: %v", err)
	}
	prefix := fstest.MapFS{}
	for _, name := range files {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		prefix[name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(ctx, db, prefix); err != nil {
		t.Fatalf("migrate through 0007: %v", err)
	}

	profileA, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "freeside-ai/candidate-repo", PRExecution: domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
		WorkflowAuditDigest: "sha256:workflow-audit",
		Review:              domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("profile A: %v", err)
	}
	inputB := domain.AutomationTrustProfileInput{
		Repo: profileA.Repo, PRExecution: profileA.PRExecution,
		CandidateAutomationChanges: profileA.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   profileA.PRGitHubTokenPermissions, AllowOIDC: true,
		CommitPlan: profileA.CommitPlan, MessageRuleset: profileA.MessageRuleset,
		WorkflowAuditDigest: profileA.WorkflowAuditDigest, Review: profileA.Review,
	}
	profileB, err := domain.NewAutomationTrustProfile(inputB)
	if err != nil {
		t.Fatalf("profile B: %v", err)
	}
	for i, profile := range []domain.AutomationTrustProfile{profileA, profileB} {
		body, err := encode(profile)
		if err != nil {
			t.Fatalf("encode profile %d: %v", i, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
			profile.ProfileDigest, profile.Repo,
			formatTime(time.Date(2026, 7, 21, 12+i, 0, 0, 0, time.UTC)), body); err != nil {
			t.Fatalf("insert profile %d: %v", i, err)
		}
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate through activation: %v", err)
	}
	s := &Store{db: db}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		current, err := tx.LatestTrustProfile(ctx, profileA.Repo)
		if err == nil && current.ProfileDigest != profileB.ProfileDigest {
			t.Errorf("backfilled digest = %q, want latest %q", current.ProfileDigest, profileB.ProfileDigest)
		}
		return err
	}); err != nil {
		t.Fatalf("read backfilled current profile: %v", err)
	}
}
