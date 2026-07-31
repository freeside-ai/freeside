package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestCandidateAuthorizationV2MigrationArchivesV1Rows(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	prefix := fstest.MapFS{}
	for _, name := range files {
		if name >= "0012_" {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		prefix[name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(ctx, db, prefix); err != nil {
		t.Fatalf("migrate through 0011: %v", err)
	}

	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "freeside-ai/candidate-repo", RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	auth, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: profile.Repo, BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe-approved",
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
		VerificationOutcome:      domain.VerificationPassed,
		TrustProfileDigest:       profile.ProfileDigest,
		InvocationID:             "verify-v1",
		CreatedAt:                time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	profileBody, err := encode(profile)
	if err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	var legacyBody map[string]any
	currentBody, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal authorization: %v", err)
	}
	if err := json.Unmarshal(currentBody, &legacyBody); err != nil {
		t.Fatalf("decode authorization fixture: %v", err)
	}
	delete(legacyBody, "evidence_snapshot_digest")
	legacyBody["id"] = "sha256:legacy-authorization"
	encodedLegacy, err := json.Marshal(legacyBody)
	if err != nil {
		t.Fatalf("encode v1 authorization: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES (?, ?, ?, ?)`,
		profile.ProfileDigest, profile.Repo, formatTime(auth.CreatedAt), profileBody,
	); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO candidate_authorizations (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"sha256:legacy-authorization", auth.Repo, auth.BaseSHA, auth.HeadSHA,
		auth.TrustProfileDigest, formatTime(auth.CreatedAt), string(encodedLegacy),
	); err != nil {
		t.Fatalf("insert v1 authorization: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO candidate_authorizations (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"sha256:corrupt-authorization", auth.Repo, auth.BaseSHA, "deadbeef",
		auth.TrustProfileDigest, formatTime(auth.CreatedAt), "not-json",
	); err != nil {
		t.Fatalf("insert corrupt authorization: %v", err)
	}
	forgedV2Marker := maps.Clone(legacyBody)
	forgedV2Marker["id"] = "sha256:forged-v2-marker"
	forgedV2Marker["head_sha"] = "feedface"
	forgedV2Marker["evidence_snapshot_digest"] = "sha256:caller-supplied"
	encodedForgedV2Marker, err := json.Marshal(forgedV2Marker)
	if err != nil {
		t.Fatalf("encode forged v2 marker: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO candidate_authorizations (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"sha256:forged-v2-marker", auth.Repo, auth.BaseSHA, "feedface",
		auth.TrustProfileDigest, formatTime(auth.CreatedAt), string(encodedForgedV2Marker),
	); err != nil {
		t.Fatalf("insert forged v2 marker: %v", err)
	}
	legacyIntent, err := json.Marshal(map[string]any{
		"identity":         "sha256:" + strings.Repeat("a", 64),
		"invocation_id":    "legacy-publication",
		"repo":             auth.Repo,
		"base_ref":         "main",
		"source_head_sha":  auth.HeadSHA,
		"authorization_id": "sha256:legacy-authorization",
	})
	if err != nil {
		t.Fatalf("encode legacy intent: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO outbox (idempotency_key, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		"publish/legacy-publication/publish.publication", "publish.publication",
		legacyIntent, formatTime(auth.CreatedAt),
	); err != nil {
		t.Fatalf("insert legacy publication intent: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate through v2 transition: %v", err)
	}
	var active, archived, corruptArchived, forgedArchived int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM candidate_authorizations`,
	).Scan(&active); err != nil {
		t.Fatalf("count active authorizations: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM legacy_candidate_authorizations WHERE id = 'sha256:legacy-authorization'`,
	).Scan(&archived); err != nil {
		t.Fatalf("count archived authorizations: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM legacy_candidate_authorizations WHERE id = 'sha256:corrupt-authorization'`,
	).Scan(&corruptArchived); err != nil {
		t.Fatalf("count corrupt archived authorizations: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM legacy_candidate_authorizations WHERE id = 'sha256:forged-v2-marker'`,
	).Scan(&forgedArchived); err != nil {
		t.Fatalf("count forged-marker archived authorizations: %v", err)
	}
	if active != 0 || archived != 1 || corruptArchived != 1 || forgedArchived != 1 {
		t.Fatalf(
			"after migration active=%d archived=%d corrupt=%d forged=%d, want 0, 1, 1, 1",
			active, archived, corruptArchived, forgedArchived,
		)
	}

	s := &Store{db: db}
	var quarantined QueueEntry
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		quarantined, err = tx.GetOutbox(
			ctx, "publish/legacy-publication/publish.publication",
		)
		return err
	}); err != nil {
		t.Fatalf("read quarantined intent: %v", err)
	}
	if !quarantined.Quarantined() {
		t.Fatalf("legacy intent status = %q, want quarantined", quarantined.Status)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		pending, err := tx.ListPendingOutbox(ctx, "publish.publication")
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Fatalf("legacy intent remained in active scan: %+v", pending)
		}
		return nil
	}); err != nil {
		t.Fatalf("list active publication intents: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordCandidateAuthorization(ctx, auth)
	}); err != nil {
		t.Fatalf("record v2 replacement: %v", err)
	}
}

func TestWorkflowAuditEvidenceMigrationPreservesLegacyLedger(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	prefix := fstest.MapFS{}
	for _, name := range files {
		if name >= "0023_" {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		prefix[name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(ctx, db, prefix); err != nil {
		t.Fatalf("migrate through 0022: %v", err)
	}
	audit := domain.WorkflowAudit{
		Repo: "freeside-ai/legacy-repo", AuditedCommitSHA: "cafebabe",
		AuditedAt:           time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: "sha256:legacy-workflow-audit",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	body, err := encode(audit)
	if err != nil {
		t.Fatalf("encode legacy audit: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO workflow_audits (repo, audited_commit_sha, audited_at, workflow_audit_digest, body)
		 VALUES (?, ?, ?, ?, ?)`,
		audit.Repo, audit.AuditedCommitSHA, formatTime(audit.AuditedAt),
		audit.WorkflowAuditDigest, body,
	); err != nil {
		t.Fatalf("insert legacy audit: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate through 0023: %v", err)
	}
	s := &Store{db: db}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		audits, err := tx.ListWorkflowAudits(ctx, audit.Repo)
		if err == nil && (len(audits) != 1 || audits[0].Audit.WorkflowAuditDigest != audit.WorkflowAuditDigest) {
			t.Fatalf("legacy audits = %+v, want preserved row", audits)
		}
		return err
	}); err != nil {
		t.Fatalf("read legacy audit: %v", err)
	}
}

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
		RepositoryID:               123456789,
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
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
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

// Captured verbatim from the v4 build: encode() of a valid profile before the
// canonical repository ID became part of the owner-approved content.
const (
	staleV4ProfileDigest = "sha256:7b0fccb74a2bd610c66339968470160a2e7f6fb17cb087ab73ce7c61916c393f"
	staleV4ProfileBody   = `{"repo":"freeside-ai/demo","pr_execution":"audited_same_repo","candidate_automation_changes":"block","pr_github_token_permissions":"read_only","allow_oidc":false,"allow_environment_secrets":false,"allow_secret_bearing_pr_jobs":false,"allow_self_hosted_ci":false,"allow_pull_request_target":false,"allow_reusable_workflows":false,"allow_package_publishing":false,"allow_artifact_consumers":false,"commit_plan":"single_commit","message_ruleset":"github/1","workflow_audit_digest":"sha256:workflow-audit","review":{"mode":"auto","config_digest":"sha256:review-config"},"protected_paths":{"extra_automation_control_patterns":["ci/*.sh","deploy/**"],"extra_reviewer_instruction_patterns":null,"extra_git_metadata_patterns":null,"extra_verification_control_patterns":["Makefile"],"extra_prompts_and_policy_patterns":["policy/**","prompts/**"],"extra_egress_and_trust_patterns":null,"extra_materiality_rules_patterns":["docs/plan.md"]},"profile_digest":"sha256:7b0fccb74a2bd610c66339968470160a2e7f6fb17cb087ab73ce7c61916c393f"}`
)

// TestTrustProfilePreRepositoryIDRowFailsClosed proves that a persisted v4
// profile cannot silently acquire a guessed repository ID after the v5
// encoding bump. JSON decoding leaves the absent field at zero, and the
// persistence-boundary re-gate rejects it before the profile can authorize
// publication or token minting.
func TestTrustProfilePreRepositoryIDRowFailsClosed(t *testing.T) {
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
		staleV4ProfileDigest, "freeside-ai/demo",
		formatTime(time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)), staleV4ProfileBody); err != nil {
		t.Fatalf("insert v4 row: %v", err)
	}

	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetTrustProfile(ctx, domain.Digest(staleV4ProfileDigest))
		return err
	})
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("pre-repository-ID profile read error = %v, want ErrNonPositive", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("stale profile surfaced as ErrNotFound; it must be a hard error: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListTrustProfiles(ctx, "freeside-ai/demo")
		return err
	})
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("pre-repository-ID profile list error = %v, want ErrNonPositive", err)
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
// Validate under v5 and surfaces as a hard error, never ErrNotFound
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
	// positive repository_id a v2 body cannot carry. Later missing policy
	// members and the digest recompute would reject it too; the first error
	// is enough to prove no v2 row is silently defaulted into a current
	// profile.
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetTrustProfile(ctx, domain.Digest(v2Digest))
		return err
	})
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("stale v2 row read error = %v, want ErrNonPositive", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("stale v2 row surfaced as ErrNotFound; a stale profile must be a hard error, not a miss: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListTrustProfiles(ctx, "freeside-ai/candidate-repo")
		return err
	})
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("stale v2 row list error = %v, want ErrNonPositive", err)
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
		RepositoryID:               123456789,
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

func TestWorkflowAuditEvidenceRetentionAndTamperGate(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}
	const repo = "freeside-ai/evidence-repo"
	newEvidence := func(marker string) domain.WorkflowAuditEvidence {
		t.Helper()
		evidence, err := domain.NewWorkflowAuditEvidence([]byte(
			`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
				`","workflows":[{"content":"` + marker + `"}]}`,
		))
		if err != nil {
			t.Fatalf("evidence %s: %v", marker, err)
		}
		return evidence
	}
	approvedEvidence := newEvidence("approved-a")
	reapprovedEvidence := newEvidence("approved-b")
	observedEvidence := newEvidence("observed")
	t0 := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	audit := func(sha string, at time.Time, evidence *domain.WorkflowAuditEvidence) domain.WorkflowAudit {
		return domain.WorkflowAudit{
			Repo: repo, AuditedCommitSHA: sha, AuditedAt: at,
			WorkflowAuditDigest: evidence.Digest(), Evidence: evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		}
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        approvedEvidence.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	reapprovedProfile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        reapprovedEvidence.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config-b",
		},
	})
	if err != nil {
		t.Fatalf("reapproved profile: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if _, err := tx.RecordWorkflowAudit(ctx, audit("approved-sha", t0, &approvedEvidence)); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
			return err
		}
		if _, err := tx.RecordWorkflowAudit(ctx, audit("reapproved-sha", t0.Add(time.Minute), &reapprovedEvidence)); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, reapprovedProfile, t0.Add(time.Minute))
	}); err != nil {
		t.Fatalf("activate retained evidence: %v", err)
	}
	var count int
	if err := db.QueryRowContext(
		ctx, `SELECT count(*) FROM workflow_audit_evidence WHERE repo = ?`, repo,
	).Scan(&count); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if count != 1 {
		t.Fatalf("retained evidence bodies after activation = %d, want one body serving both roles", count)
	}
	var supersededCount int
	if err := db.QueryRowContext(
		ctx,
		`SELECT count(*) FROM workflow_audit_evidence WHERE repo = ? AND workflow_audit_digest = ?`,
		repo, approvedEvidence.Digest(),
	).Scan(&supersededCount); err != nil {
		t.Fatalf("count superseded evidence: %v", err)
	}
	if supersededCount != 0 {
		t.Fatalf("superseded approved evidence bodies = %d, want pruned on activation", supersededCount)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit("observed-sha", t0.Add(2*time.Minute), &observedEvidence))
		return err
	}); err != nil {
		t.Fatalf("record observed evidence: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.ActivateTrustProfile(ctx, repo, profile.ProfileDigest, t0.Add(3*time.Minute))
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activate profile with pruned evidence error = %v, want ErrNotFound", err)
	}
	if err := db.QueryRowContext(
		ctx, `SELECT count(*) FROM workflow_audit_evidence WHERE repo = ?`, repo,
	).Scan(&count); err != nil {
		t.Fatalf("recount evidence: %v", err)
	}
	if count != 2 {
		t.Fatalf("retained evidence bodies = %d, want active approved plus latest observed", count)
	}

	const tamperedNeedle = "tampered-workflow-content-must-not-leak"
	tampered := []byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
			`","workflows":[{"content":"` + tamperedNeedle + `"}]}`,
	)
	if _, err := db.ExecContext(
		ctx,
		`UPDATE workflow_audit_evidence SET body = ? WHERE repo = ? AND workflow_audit_digest = ?`,
		tampered, repo, observedEvidence.Digest(),
	); err != nil {
		t.Fatalf("tamper evidence: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.WorkflowAuditReview(ctx, repo)
		return err
	})
	if !errors.Is(err, domain.ErrWorkflowAuditEvidenceMismatch) {
		t.Fatalf("tampered review error = %v, want ErrWorkflowAuditEvidenceMismatch", err)
	}
	if strings.Contains(err.Error(), tamperedNeedle) {
		t.Fatalf("tampered evidence leaked through error: %v", err)
	}
}

func TestWorkflowAuditEvidencePruningRejectsUnauthenticatedBindings(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}
	const repo = "freeside-ai/evidence-repo"
	newEvidence := func(marker string) domain.WorkflowAuditEvidence {
		t.Helper()
		evidence, err := domain.NewWorkflowAuditEvidence([]byte(
			`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
				`","workflows":[{"content":"` + marker + `"}]}`,
		))
		if err != nil {
			t.Fatalf("evidence %s: %v", marker, err)
		}
		return evidence
	}
	approved := newEvidence("approved")
	intermediate := newEvidence("intermediate")
	latest := newEvidence("latest")
	t0 := time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
	audit := func(sha string, at time.Time, evidence *domain.WorkflowAuditEvidence) domain.WorkflowAudit {
		return domain.WorkflowAudit{
			Repo: repo, AuditedCommitSHA: sha, AuditedAt: at,
			WorkflowAuditDigest: evidence.Digest(), Evidence: evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		}
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        approved.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if _, err := tx.RecordWorkflowAudit(ctx, audit("approved", t0, &approved)); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
			return err
		}
		_, err := tx.RecordWorkflowAudit(ctx, audit("intermediate", t0.Add(time.Minute), &intermediate))
		return err
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	var profileBody string
	if err := db.QueryRowContext(
		ctx, `SELECT body FROM trust_profiles WHERE profile_digest = ?`, profile.ProfileDigest,
	).Scan(&profileBody); err != nil {
		t.Fatalf("read active profile body: %v", err)
	}
	tamperedProfileBody := strings.Replace(
		profileBody, string(approved.Digest()), string(intermediate.Digest()), 1,
	)
	if tamperedProfileBody == profileBody {
		t.Fatal("profile tamper fixture did not change the body")
	}
	if _, err := db.ExecContext(
		ctx, `UPDATE trust_profiles SET body = ? WHERE profile_digest = ?`,
		tamperedProfileBody, profile.ProfileDigest,
	); err != nil {
		t.Fatalf("tamper active profile body: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit("latest", t0.Add(2*time.Minute), &latest))
		return err
	}); err != nil {
		t.Fatalf("record audit over tampered active profile: %v", err)
	}
	activationTampered := newEvidence("activation-tampered-latest")
	if _, err := db.ExecContext(
		ctx, `UPDATE trust_profiles SET body = ? WHERE profile_digest = ?`,
		profileBody, profile.ProfileDigest,
	); err != nil {
		t.Fatalf("restore active profile body: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`UPDATE trust_profile_activations SET workflow_audit_digest = ?
		 WHERE id = (SELECT max(id) FROM trust_profile_activations WHERE repo = ?)`,
		intermediate.Digest(), repo,
	); err != nil {
		t.Fatalf("tamper activation binding: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordWorkflowAudit(
			ctx, audit("activation-tampered-latest", t0.Add(3*time.Minute), &activationTampered),
		)
		return err
	}); err != nil {
		t.Fatalf("record audit over tampered activation: %v", err)
	}
	for _, tt := range []struct {
		name   string
		digest domain.Digest
		want   int
	}{
		{"approved", approved.Digest(), 1},
		{"intermediate", intermediate.Digest(), 1},
		{"latest", latest.Digest(), 1},
		{"activation-tampered latest", activationTampered.Digest(), 1},
	} {
		var count int
		if err := db.QueryRowContext(
			ctx,
			`SELECT count(*) FROM workflow_audit_evidence
			 WHERE repo = ? AND workflow_audit_digest = ?`,
			repo, tt.digest,
		).Scan(&count); err != nil {
			t.Fatalf("count %s evidence: %v", tt.name, err)
		}
		if count != tt.want {
			t.Fatalf("%s evidence count = %d, want %d", tt.name, count, tt.want)
		}
	}

	if _, err := db.ExecContext(
		ctx,
		`UPDATE workflow_audits SET workflow_audit_digest = ?
		 WHERE repo = ? AND audited_commit_sha = ?`,
		intermediate.Digest(), repo, "activation-tampered-latest",
	); err != nil {
		t.Fatalf("tamper latest audit binding: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.pruneWorkflowAuditEvidence(ctx, repo, approved.Digest())
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("prune over tampered latest audit error = %v, want errRowInconsistent", err)
	}
	var retained int
	if err := db.QueryRowContext(
		ctx, `SELECT count(*) FROM workflow_audit_evidence WHERE repo = ?`, repo,
	).Scan(&retained); err != nil {
		t.Fatalf("count evidence after rejected prune: %v", err)
	}
	if retained != 4 {
		t.Fatalf("evidence after rejected prune = %d, want all 4 bodies retained", retained)
	}
	if _, err := db.ExecContext(
		ctx,
		`UPDATE workflow_audits SET workflow_audit_digest = ?
		 WHERE repo = ? AND audited_commit_sha = ?`,
		activationTampered.Digest(), repo, "activation-tampered-latest",
	); err != nil {
		t.Fatalf("restore latest audit binding: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`UPDATE workflow_audits SET workflow_audit_digest = ?
		 WHERE repo = ? AND audited_commit_sha = ?`,
		intermediate.Digest(), repo, "approved",
	); err != nil {
		t.Fatalf("tamper approved audit binding: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.DeleteWorkflowAuditEvidence(ctx, repo)
	}); err != nil {
		t.Fatalf("delete evidence before reactivation: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.ActivateTrustProfile(ctx, repo, profile.ProfileDigest, t0.Add(4*time.Minute))
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("activate over tampered audit error = %v, want errRowInconsistent", err)
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
		`INSERT INTO trust_profile_activations
		 (repo, profile_digest, workflow_audit_digest, activated_at)
		 VALUES (?, ?, ?, ?)`,
		"freeside-ai/candidate-repo", staleV2ProfileDigest, "sha256:workflow-audit",
		formatTime(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("activate v2 row: %v", err)
	}
	freshEvidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"freeside-ai/candidate-repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatalf("fresh evidence: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, domain.WorkflowAudit{
			Repo: "freeside-ai/candidate-repo", AuditedCommitSHA: "fresh-sha",
			AuditedAt:           time.Date(2026, 7, 21, 12, 30, 0, 0, time.UTC),
			WorkflowAuditDigest: freshEvidence.Digest(),
			Evidence:            &freshEvidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		})
		return err
	}); err != nil {
		t.Fatalf("record fresh audit over stale active profile: %v", err)
	}

	// Before re-approval the stale row is the newest: still fail closed.
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.LatestTrustProfile(ctx, "freeside-ai/candidate-repo")
		return err
	})
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("latest over only a stale row error = %v, want ErrNonPositive", err)
	}

	// The owner records the re-approved current profile: same content plus the
	// explicit policy keys, a new digest, a new row.
	reapproved, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/candidate-repo",
		RepositoryID:               123456789,
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
	if !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("history list after re-approval error = %v, want ErrNonPositive", err)
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
		Repo: "freeside-ai/candidate-repo", RepositoryID: 123456789, PRExecution: domain.PRExecutionAuditedSameRepo,
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
		Repo: profileA.Repo, RepositoryID: profileA.RepositoryID, PRExecution: profileA.PRExecution,
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
	var workflowDigest domain.Digest
	if err := db.QueryRowContext(
		ctx,
		`SELECT workflow_audit_digest FROM trust_profile_activations
		 WHERE repo = ? ORDER BY id DESC LIMIT 1`,
		profileA.Repo,
	).Scan(&workflowDigest); err != nil {
		t.Fatalf("read backfilled workflow-audit binding: %v", err)
	}
	if workflowDigest != profileB.WorkflowAuditDigest {
		t.Fatalf(
			"backfilled workflow-audit digest = %q, want %q",
			workflowDigest, profileB.WorkflowAuditDigest,
		)
	}
}
