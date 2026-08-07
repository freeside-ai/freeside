package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func trustProfileFixture(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/candidate-repo",
		RepositoryID:               123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraVerificationControlPatterns: []string{"Makefile"},
		},
	})
	if err != nil {
		t.Fatalf("trust profile fixture: %v", err)
	}
	return profile
}

func authorizationFixture(t *testing.T, profile domain.AutomationTrustProfile, invocation domain.InvocationID) domain.CandidateAuthorization {
	t.Helper()
	a, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: profile.Repo, BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe-approved",
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
		VerificationOutcome:      domain.VerificationPassed,
		Findings: []domain.CandidateFinding{{
			Class: domain.FindingClassRepoChangePolicy, Origin: domain.FindingOriginImport,
			Kind: "size_violation", Path: "assets/big.bin",
			Disposition: domain.DispositionBlocking,
		}},
		TrustProfileDigest: profile.ProfileDigest,
		InvocationID:       invocation,
		CreatedAt:          time.Date(2026, 7, 18, 12, 0, 0, 123456789, time.UTC),
	})
	if err != nil {
		t.Fatalf("authorization fixture: %v", err)
	}
	return a
}

func workflowAuditEvidenceFixture(t *testing.T, repo, marker string) domain.WorkflowAuditEvidence {
	t.Helper()
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/test","repo":"` + repo +
			`","workflows":[{"path":".github/workflows/` + marker + `.yml"}]}`,
	))
	if err != nil {
		t.Fatalf("workflow audit evidence fixture: %v", err)
	}
	return evidence
}

// TestTrustProfileRoundTrip: a recorded profile reads back identical by
// digest and by repo listing, a byte-identical replay converges on the one
// row, and a revised profile is a second row under its own digest.
func TestTrustProfileRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	profile := trustProfileFixture(t)
	recordedAt := time.Date(2026, 7, 18, 11, 0, 0, 0, time.FixedZone("PDT", -7*60*60))

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, recordedAt); err != nil {
			return err
		}
		// Replay converges: same content, same digest, no conflict.
		return tx.RecordTrustProfile(ctx, profile, recordedAt)
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	revised := profile
	revised.AllowOIDC = true
	revisedInput := domain.AutomationTrustProfileInput{
		Repo:                       revised.Repo,
		RepositoryID:               revised.RepositoryID,
		PRExecution:                revised.PRExecution,
		CandidateAutomationChanges: revised.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   revised.PRGitHubTokenPermissions,
		CommitPlan:                 revised.CommitPlan,
		MessageRuleset:             revised.MessageRuleset,
		AllowOIDC:                  true,
		WorkflowAuditDigest:        revised.WorkflowAuditDigest,
		Review:                     revised.Review,
		ProtectedPaths:             revised.ProtectedPaths,
	}
	revisedProfile, err := domain.NewAutomationTrustProfile(revisedInput)
	if err != nil {
		t.Fatalf("revised profile: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, revisedProfile, recordedAt.Add(time.Hour))
	})
	if err != nil {
		t.Fatalf("record revised: %v", err)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetTrustProfile(ctx, profile.ProfileDigest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, profile) {
			t.Errorf("get round-trip mismatch:\ngot  %+v\nwant %+v", got, profile)
		}
		recs, err := tx.ListTrustProfiles(ctx, profile.Repo)
		if err != nil {
			return err
		}
		if len(recs) != 2 {
			t.Fatalf("listed %d profiles, want 2", len(recs))
		}
		if recs[0].Profile.ProfileDigest != profile.ProfileDigest ||
			recs[1].Profile.ProfileDigest != revisedProfile.ProfileDigest {
			t.Errorf("list order: %q then %q, want original then revised", recs[0].Profile.ProfileDigest, recs[1].Profile.ProfileDigest)
		}
		if !recs[0].RecordedAt.Equal(recordedAt) || recs[0].RecordedAt.Location() != time.UTC {
			t.Errorf("recorded_at %v, want UTC instant %v", recs[0].RecordedAt, recordedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestTrustProfileExactReactivation proves that current selection is an
// owner-decision axis rather than profile insertion order: A -> B -> A is
// representable without mutating immutable profile content, while replaying
// RecordTrustProfile(A) after B remains inert and cannot resurrect A.
func TestTrustProfileExactReactivation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	profileA := trustProfileFixture(t)
	inB := domain.AutomationTrustProfileInput{
		Repo: profileA.Repo, RepositoryID: profileA.RepositoryID, PRExecution: profileA.PRExecution,
		CandidateAutomationChanges: profileA.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   profileA.PRGitHubTokenPermissions,
		AllowOIDC:                  true,
		CommitPlan:                 profileA.CommitPlan,
		MessageRuleset:             profileA.MessageRuleset,
		WorkflowAuditDigest:        profileA.WorkflowAuditDigest,
		Review:                     profileA.Review,
		ProtectedPaths:             profileA.ProtectedPaths,
	}
	profileB, err := domain.NewAutomationTrustProfile(inB)
	if err != nil {
		t.Fatalf("profile B: %v", err)
	}
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profileA, t0); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, profileB, t0.Add(time.Minute))
	}); err != nil {
		t.Fatalf("record A then B: %v", err)
	}
	// A stale retry is content persistence, not a new owner decision.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profileA, t0)
	}); err != nil {
		t.Fatalf("replay A: %v", err)
	}
	assertCurrent := func(want domain.Digest) {
		t.Helper()
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			got, err := tx.LatestTrustProfile(ctx, profileA.Repo)
			if err == nil && got.ProfileDigest != want {
				t.Errorf("current digest = %q, want %q", got.ProfileDigest, want)
			}
			return err
		}); err != nil {
			t.Fatalf("read current: %v", err)
		}
	}
	assertCurrent(profileB.ProfileDigest)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ActivateTrustProfile(ctx, profileA.Repo, profileA.ProfileDigest, t0.Add(2*time.Minute))
	}); err != nil {
		t.Fatalf("reactivate A: %v", err)
	}
	assertCurrent(profileA.ProfileDigest)
}

// TestWorkflowAuditAppendOnly: audits are an observation ledger — two
// identical observations are two rows, read back field-identical in
// insertion order.
func TestWorkflowAuditAppendOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	evidence := workflowAuditEvidenceFixture(t, "freeside-ai/candidate-repo", "ci")
	audit := domain.WorkflowAudit{
		Repo:                "freeside-ai/candidate-repo",
		AuditedCommitSHA:    "cafebabe",
		AuditedAt:           time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(),
		Evidence:            &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadWrite,
		SelfHostedRunners:   true,
	}

	var recorded []store.WorkflowAuditRecord
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for range 2 {
			rec, err := tx.RecordWorkflowAudit(ctx, audit)
			if err != nil {
				return err
			}
			recorded = append(recorded, rec)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if recorded[0].ID <= 0 || recorded[1].ID <= recorded[0].ID {
		t.Fatalf("assigned IDs not ascending: %d, %d", recorded[0].ID, recorded[1].ID)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		recs, err := tx.ListWorkflowAudits(ctx, audit.Repo)
		if err != nil {
			return err
		}
		if len(recs) != 2 {
			t.Fatalf("listed %d audits, want 2", len(recs))
		}
		for i, rec := range recs {
			if rec.ID != recorded[i].ID {
				t.Errorf("audit %d: ID %d, want %d", i, rec.ID, recorded[i].ID)
			}
			if !rec.Audit.AuditedAt.Equal(audit.AuditedAt) {
				t.Errorf("audit %d: audited_at %v, want %v", i, rec.Audit.AuditedAt, audit.AuditedAt)
			}
			got, want := rec.Audit, audit
			got.Evidence, want.Evidence = nil, nil
			got.AuditedAt, want.AuditedAt = time.Time{}, time.Time{}
			if got != want {
				t.Errorf("audit %d round-trip mismatch:\ngot  %+v\nwant %+v", i, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestWorkflowAuditReviewRetentionAndDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	const (
		repo   = "freeside-ai/candidate-repo"
		needle = "workflow-review-only-needle"
	)
	approvedEvidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
			`","workflows":[{"path":".github/workflows/ci.yml","content":"` + needle +
			`"}],"dynamic_workflows":[],"branch_protection":{"enforced":true}}`,
	))
	if err != nil {
		t.Fatalf("approved evidence: %v", err)
	}
	intermediateEvidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
			`","workflows":[{"path":".github/workflows/ci.yml","content":"` + needle +
			`"}],"dynamic_workflows":[{"path":"dynamic/old","state":"active"}],"branch_protection":{"enforced":true}}`,
	))
	if err != nil {
		t.Fatalf("intermediate evidence: %v", err)
	}
	observedEvidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
			`","workflows":[{"path":".github/workflows/ci.yml","content":"` + needle +
			`"}],"dynamic_workflows":[{"path":"dynamic/codeql","state":"active"}],"branch_protection":{"enforced":false}}`,
	))
	if err != nil {
		t.Fatalf("observed evidence: %v", err)
	}
	audit := func(at time.Time, evidence *domain.WorkflowAuditEvidence) domain.WorkflowAudit {
		return domain.WorkflowAudit{
			Repo: repo, AuditedCommitSHA: fmt.Sprintf("commit-%d", at.Minute()),
			AuditedAt: at, WorkflowAuditDigest: evidence.Digest(), Evidence: evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		}
	}
	t0 := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	approvedAudit := audit(t0, &approvedEvidence)
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        approvedEvidence.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, approvedAudit)
		return err
	}); err != nil {
		t.Fatalf("record proposed-profile audit: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		proposed, err := tx.WorkflowAuditReviewForProfile(ctx, profile)
		if err == nil && (proposed.Approved.Audit.ID != proposed.Observed.Audit.ID ||
			len(proposed.ChangedFields) != 0) {
			t.Fatalf("proposed review = %+v, want one unchanged digest-bound observation", proposed)
		}
		return err
	}); err != nil {
		t.Fatalf("proposed profile review: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
			return err
		}
		if _, err := tx.RecordWorkflowAudit(ctx, audit(t0.Add(time.Minute), &intermediateEvidence)); err != nil {
			return err
		}
		_, err := tx.RecordWorkflowAudit(ctx, audit(t0.Add(2*time.Minute), &observedEvidence))
		return err
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}

	var review store.WorkflowAuditReview
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		review, err = tx.WorkflowAuditReview(ctx, repo)
		return err
	}); err != nil {
		t.Fatalf("WorkflowAuditReview: %v", err)
	}
	if review.Approved.Audit.Audit.WorkflowAuditDigest != approvedEvidence.Digest() ||
		review.Observed.Audit.Audit.WorkflowAuditDigest != observedEvidence.Digest() {
		t.Fatalf("review selected wrong sides: %+v", review)
	}
	if want := []string{"branch_protection", "dynamic_workflows"}; !reflect.DeepEqual(review.ChangedFields, want) {
		t.Fatalf("changed fields = %v, want %v", review.ChangedFields, want)
	}
	projected, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	if !strings.Contains(string(projected), needle) {
		t.Fatalf("review JSON omitted evidence: %s", projected)
	}
	if rendered := fmt.Sprintf("%+v", review); strings.Contains(rendered, needle) {
		t.Fatalf("ordinary review formatting leaked evidence: %s", rendered)
	}

	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.DeleteWorkflowAuditEvidence(ctx, repo)
	}); err != nil {
		t.Fatalf("delete evidence: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.WorkflowAuditReview(ctx, repo)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("review after deletion error = %v, want ErrNotFound", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		audits, err := tx.ListWorkflowAudits(ctx, repo)
		if err == nil && len(audits) != 3 {
			t.Fatalf("audit ledger rows = %d, want 3 after evidence deletion", len(audits))
		}
		return err
	}); err != nil {
		t.Fatalf("audit ledger after deletion: %v", err)
	}
}

// TestCandidateAuthorizationRoundTrip: a recorded authorization reads back
// identical by id and by (repo, head) listing, and a byte-identical replay
// converges on the one row.
func TestCandidateAuthorizationRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	profile := trustProfileFixture(t)
	auth := authorizationFixture(t, profile, "inv-1")

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, auth.CreatedAt); err != nil {
			return err
		}
		if err := tx.RecordCandidateAuthorization(ctx, auth); err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(ctx, auth)
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetCandidateAuthorization(ctx, auth.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, auth) {
			t.Errorf("get round-trip mismatch:\ngot  %+v\nwant %+v", got, auth)
		}
		listed, err := tx.ListCandidateAuthorizations(ctx, auth.Repo, auth.HeadSHA)
		if err != nil {
			return err
		}
		if len(listed) != 1 || !reflect.DeepEqual(listed[0], auth) {
			t.Errorf("list = %+v, want exactly the recorded authorization", listed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestAuthorizationRequiresProfileRow: the composite foreign key fails
// closed — an authorization bound to a profile digest the store has never
// recorded cannot exist (publication trust never dangles from a missing
// profile), and neither can one bound to a profile recorded for a
// *different* repository (one repository's candidates never bind another's
// automation posture).
func TestAuthorizationRequiresProfileRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	profile := trustProfileFixture(t)
	auth := authorizationFixture(t, profile, "inv-1")

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCandidateAuthorization(ctx, auth)
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("authorization without profile row: error = %v, want foreign-key failure", err)
	}

	// Cross-repo reuse: the profile row exists, but for another repository.
	// The digest alone matches; the (repo, digest) pair must not.
	crossRepo, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/other-repo", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: "sha256:recipe-approved",
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
		VerificationOutcome:      domain.VerificationPassed,
		TrustProfileDigest:       profile.ProfileDigest,
		InvocationID:             "inv-1",
		CreatedAt:                time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("cross-repo authorization fixture: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, crossRepo.CreatedAt); err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(ctx, crossRepo)
	})
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("authorization against another repo's profile: error = %v, want foreign-key failure", err)
	}
}

// TestAuthorizationUniquePerHeadAndProfile pins the owner decision on the
// uniqueness key: one authorization per (repo, head, profile), so a second,
// different record for the same binding fails loudly, while the same head
// re-authorized under a revised (re-recorded) profile is a legal second row.
func TestAuthorizationUniquePerHeadAndProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	profile := trustProfileFixture(t)
	first := authorizationFixture(t, profile, "inv-1")
	// Same head, same profile, different producing invocation: a distinct
	// record (different content id) for a binding that already holds one.
	second := authorizationFixture(t, profile, "inv-2")
	if first.ID == second.ID {
		t.Fatal("fixtures unexpectedly share an id")
	}

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, first.CreatedAt); err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(ctx, first)
	})
	if err != nil {
		t.Fatalf("record first: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCandidateAuthorization(ctx, second)
	})
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second authorization for one (repo, head, profile): error = %v, want unique-constraint failure", err)
	}

	// The same head under a revised profile is the §5.5 drift-recovery path
	// and must be representable.
	revisedInput := domain.AutomationTrustProfileInput{
		Repo:                       profile.Repo,
		RepositoryID:               profile.RepositoryID,
		PRExecution:                profile.PRExecution,
		CandidateAutomationChanges: profile.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   profile.PRGitHubTokenPermissions,
		CommitPlan:                 profile.CommitPlan,
		MessageRuleset:             profile.MessageRuleset,
		AllowOIDC:                  true,
		WorkflowAuditDigest:        profile.WorkflowAuditDigest,
		Review:                     profile.Review,
		ProtectedPaths:             profile.ProtectedPaths,
	}
	revisedProfile, err := domain.NewAutomationTrustProfile(revisedInput)
	if err != nil {
		t.Fatalf("revised profile: %v", err)
	}
	reauthorized := authorizationFixture(t, revisedProfile, "inv-3")
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, revisedProfile, first.CreatedAt.Add(time.Hour)); err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(ctx, reauthorized)
	})
	if err != nil {
		t.Fatalf("re-authorization under revised profile: %v", err)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		listed, err := tx.ListCandidateAuthorizations(ctx, first.Repo, first.HeadSHA)
		if err != nil {
			return err
		}
		if len(listed) != 2 {
			t.Fatalf("listed %d authorizations for the head, want 2 (one per profile)", len(listed))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}
