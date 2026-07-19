package publish_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testTrustRepo is the repository testCandidate publishes to; its trust
// fixtures below are what the drift gate checks the candidate against.
const testTrustRepo = "freeside-ai/evidence-repo"

// trustProfileForRepo builds a conformant, human-approved trust profile for
// repo via the trusted constructor: read_only tokens, no OIDC/secret/self-
// hosted/pull_request_target allowance, bound to a fixed audit digest.
func trustProfileForRepo(t *testing.T, repo string) domain.AutomationTrustProfile {
	t.Helper()
	p, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       repo,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:workflow-audit-fixture",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("trustProfileForRepo: %v", err)
	}
	return p
}

// workflowAuditForRepo builds the conformant audit that matches
// trustProfileForRepo: the bound surface digest, read_only tokens, and no
// privilege the profile does not allow.
func workflowAuditForRepo(t *testing.T, repo string) domain.WorkflowAudit {
	t.Helper()
	return domain.WorkflowAudit{
		Repo:                repo,
		AuditedCommitSHA:    testHeadSHA,
		AuditedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		WorkflowAuditDigest: "sha256:workflow-audit-fixture",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
}

func testTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	return trustProfileForRepo(t, testTrustRepo)
}

// testTrustProfileDigest is the digest testCandidate binds to; the
// conformant trust source and seedTrust both resolve to the same profile.
func testTrustProfileDigest(t *testing.T) domain.Digest {
	t.Helper()
	return testTrustProfile(t).ProfileDigest
}

func testWorkflowAudit(t *testing.T) domain.WorkflowAudit {
	t.Helper()
	return workflowAuditForRepo(t, testTrustRepo)
}

// memoryTrustSource is the in-memory TrustSource fake, mirroring
// memoryLedger: a fixed current profile/audit plus injectable failure. A nil
// profile or audit models "none recorded", which the drift gate fails closed
// on. The repo argument is ignored: a fixture is built for one repository.
type memoryTrustSource struct {
	profile *domain.AutomationTrustProfile
	audit   *domain.WorkflowAudit
	err     error
}

func (s memoryTrustSource) CurrentTrust(context.Context, string) (publish.CurrentTrust, error) {
	if s.err != nil {
		return publish.CurrentTrust{}, s.err
	}
	return publish.CurrentTrust{Profile: s.profile, Audit: s.audit}, nil
}

var _ publish.TrustSource = memoryTrustSource{}

// conformantTrust is the memory trust source testCandidate passes the drift
// gate against: the current profile it is bound to and a matching audit.
func conformantTrust(t *testing.T) memoryTrustSource {
	t.Helper()
	p := testTrustProfile(t)
	a := testWorkflowAudit(t)
	return memoryTrustSource{profile: &p, audit: &a}
}

// seedTrust records a conformant profile and audit for repo into a store,
// so a store-backed publisher (openKillHarness) passes the drift gate. It
// returns the recorded profile's digest, which a candidate must bind to.
func seedTrust(t *testing.T, s *store.Store, repo string) domain.Digest {
	t.Helper()
	ctx := context.Background()
	profile := trustProfileForRepo(t, repo)
	audit := workflowAuditForRepo(t, repo)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, audit.AuditedAt); err != nil {
			return err
		}
		_, err := tx.RecordWorkflowAudit(ctx, audit)
		return err
	}); err != nil {
		t.Fatalf("seedTrust: %v", err)
	}
	return profile.ProfileDigest
}

// TestStoreTrustSourceReturnsLatest: the store-backed source reports no
// profile/audit for an unseeded repository (absence the gate fails closed
// on) and the recorded profile/audit once seeded.
func TestStoreTrustSourceReturnsLatest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	src, err := publish.NewStoreTrustSource(s)
	if err != nil {
		t.Fatalf("NewStoreTrustSource: %v", err)
	}

	ct, err := src.CurrentTrust(ctx, testTrustRepo)
	if err != nil {
		t.Fatalf("CurrentTrust empty: %v", err)
	}
	if ct.Profile != nil || ct.Audit != nil {
		t.Fatalf("empty store returned profile=%v audit=%v, want both nil", ct.Profile, ct.Audit)
	}

	digest := seedTrust(t, s, testTrustRepo)
	ct, err = src.CurrentTrust(ctx, testTrustRepo)
	if err != nil {
		t.Fatalf("CurrentTrust seeded: %v", err)
	}
	if ct.Profile == nil || ct.Profile.ProfileDigest != digest {
		t.Fatalf("current profile = %v, want digest %s", ct.Profile, digest)
	}
	if ct.Audit == nil || ct.Audit.WorkflowAuditDigest != "sha256:workflow-audit-fixture" {
		t.Fatalf("current audit = %v, want the seeded audit", ct.Audit)
	}
}
