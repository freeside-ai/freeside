package publish_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testBaseSHA is the base commit the authorization binds. The candidate
// targets a base *ref* ("main"); the authorization binds the resolved base
// *sha*, a distinct coordinate the gate does not cross-check (publisher.go).
const testBaseSHA = "1111111111111111111111111111111111111111"

// authorizingInput is the caller-supplied side of the conformant
// authorization testCandidate binds to: the same repo/head/recipe/trust
// profile the candidate publishes under, a passed verification, and no
// finding. The producing (verification) invocation is deliberately distinct
// from the publishing invocation the candidate carries.
func authorizingInput(t *testing.T) domain.CandidateAuthorizationInput {
	t.Helper()
	return domain.CandidateAuthorizationInput{
		Repo:                     testTrustRepo,
		BaseSHA:                  testBaseSHA,
		HeadSHA:                  testHeadSHA,
		ImportResultDigest:       "sha256:import-result-fixture",
		VerificationRecipeDigest: testRecipe,
		VerificationOutcome:      domain.VerificationPassed,
		Findings:                 nil,
		TrustProfileDigest:       testTrustProfileDigest(t),
		InvocationID:             "inv-verify-0001",
		CreatedAt:                time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// newAuthorization builds a validated authorization from in, failing the test
// on any malformed fixture.
func newAuthorization(t *testing.T, in domain.CandidateAuthorizationInput) domain.CandidateAuthorization {
	t.Helper()
	a, err := domain.NewCandidateAuthorization(in)
	if err != nil {
		t.Fatalf("NewCandidateAuthorization: %v", err)
	}
	return a
}

// testCandidateAuthorization is the conformant, publication-authorizing record
// testCandidate names. Its content id is the candidate's AuthorizationID.
func testCandidateAuthorization(t *testing.T) domain.CandidateAuthorization {
	t.Helper()
	return newAuthorization(t, authorizingInput(t))
}

// memoryAuthorizationSource is the in-memory AuthorizationSource fake,
// mirroring memoryTrustSource: a fixed id→record map plus an injectable
// failure. An id absent from the map models "none recorded", which the gate
// fails closed on.
type memoryAuthorizationSource struct {
	auths map[domain.Digest]domain.CandidateAuthorization
	err   error
}

func (s memoryAuthorizationSource) Authorization(_ context.Context, id domain.Digest) (domain.CandidateAuthorization, bool, error) {
	if s.err != nil {
		return domain.CandidateAuthorization{}, false, s.err
	}
	a, ok := s.auths[id]
	return a, ok, nil
}

var _ publish.AuthorizationSource = memoryAuthorizationSource{}

// authzWith is a memory source holding exactly the given authorizations.
func authzWith(auths ...domain.CandidateAuthorization) memoryAuthorizationSource {
	m := make(map[domain.Digest]domain.CandidateAuthorization, len(auths))
	for _, a := range auths {
		m[a.ID] = a
	}
	return memoryAuthorizationSource{auths: m}
}

// conformantAuthz is the memory source testCandidate passes the authorization
// gate against: it holds the record the candidate's AuthorizationID names.
func conformantAuthz(t *testing.T) memoryAuthorizationSource {
	t.Helper()
	return authzWith(testCandidateAuthorization(t))
}

// seedAuthz records a into a store so a store-backed authorization source
// (openKillHarness) resolves it. The (repo, trust-profile) pair must already
// be recorded (seedTrust), so call it after seedTrust.
func seedAuthz(t *testing.T, s *store.Store, a domain.CandidateAuthorization) {
	t.Helper()
	ctx := context.Background()
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCandidateAuthorization(ctx, a)
	}); err != nil {
		t.Fatalf("seedAuthz: %v", err)
	}
}

// TestStoreAuthorizationSourceResolves: the store-backed source reports
// found=false for an unrecorded id (absence the gate fails closed on) and the
// recorded authorization once seeded.
func TestStoreAuthorizationSourceResolves(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	src, err := publish.NewStoreAuthorizationSource(s)
	if err != nil {
		t.Fatalf("NewStoreAuthorizationSource: %v", err)
	}

	auth := testCandidateAuthorization(t)
	if _, found, err := src.Authorization(ctx, auth.ID); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v, want found=false nil", found, err)
	}

	seedTrust(t, s, testTrustRepo)
	seedAuthz(t, s, auth)
	got, found, err := src.Authorization(ctx, auth.ID)
	if err != nil {
		t.Fatalf("Authorization seeded: %v", err)
	}
	if !found || got.ID != auth.ID {
		t.Fatalf("resolved found=%v id=%s, want true %s", found, got.ID, auth.ID)
	}
}
