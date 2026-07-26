package publish_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// fakeResolver stands in for the Wave 2 engine: it hands the drain back
// a fixed candidate (and approved-recipe set), the way the engine would
// reload it from workflow state. err injects a resolution failure.
type fakeResolver struct {
	cand        publish.Candidate
	approved    map[domain.Digest]bool
	publishHead func(context.Context, publish.IdentityInput) error
	err         error
}

func (r fakeResolver) Resolve(context.Context, publish.Intent) (publish.RecoveryCandidate, error) {
	return publish.RecoveryCandidate{
		Candidate: r.cand, ApprovedRecipes: r.approved, PublishHead: r.publishHead,
	}, r.err
}

var _ publish.CandidateResolver = fakeResolver{}

// drainHarness bundles a store-backed publisher over one fakeGitHub.
type drainHarness struct {
	store  *store.Store
	gh     *fakeGitHub
	ledger *publish.StoreLedger
	pub    *publish.Publisher
}

type mutableWorkflowAuditor struct {
	audit *domain.WorkflowAudit
}

func (a mutableWorkflowAuditor) Audit(context.Context, string, string) (domain.WorkflowAudit, error) {
	return *a.audit, nil
}

func newDrainHarness(t *testing.T) drainHarness {
	return newDrainHarnessWithTrust(t, conformantTrust(t))
}

// newDrainHarnessWithTrust builds a drain harness whose publisher reads
// automation trust from the given source, so a test can drive the drift
// gate during recovery.
func newDrainHarnessWithTrust(t *testing.T, trust publish.TrustSource) drainHarness {
	t.Helper()
	s := newTestStore(t)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	gh := newFakeGitHub(t)
	return drainHarness{store: s, gh: gh, ledger: ledger, pub: newTestPublisherWithTrust(t, gh, ledger, trust)}
}

func resolverFor(t *testing.T, c publish.Candidate) fakeResolver {
	t.Helper()
	return fakeResolver{
		cand: c, approved: testApprovedRecipes(),
		publishHead: func(context.Context, publish.IdentityInput) error { return nil },
	}
}

// testCandidateIdentity derives the identity a clean publish of
// testCandidate converges to, using the same exported derivation an
// external caller would.
func testCandidateIdentity(t *testing.T) publish.Identity {
	t.Helper()
	recipe := testRecipe
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    &recipe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countRequests(log []string, want string) int {
	n := 0
	for _, r := range log {
		if r == want {
			n++
		}
	}
	return n
}

func pendingPublications(t *testing.T, s *store.Store) []store.QueueEntry {
	t.Helper()
	var pending []store.QueueEntry
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(context.Background(), publish.IntentKindPublication)
		pending = entries
		return err
	}); err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return pending
}

// assertOutcomeRecorded probes the inbox by idempotent re-insert: exactly
// one outcome must already be committed under key, carrying the expected
// payload. A probe that inserts (nothing was there) or finds a different
// payload fails.
func assertOutcomeRecorded(t *testing.T, s *store.Store, key string, want publish.Outcome) {
	t.Helper()
	payload, err := want.Encode()
	if err != nil {
		t.Fatalf("encode expected outcome: %v", err)
	}
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		entry, inserted, err := tx.RecordInbox(context.Background(), key, publish.IntentKindOutcome, payload)
		if err != nil {
			return err
		}
		if inserted {
			return fmt.Errorf("no outcome was recorded under %s", key)
		}
		if !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("recorded outcome = %s, want %s", entry.Payload, payload)
		}
		return nil
	}); err != nil {
		t.Errorf("outcome assertion: %v", err)
	}
}

// TestDrainConvergesPendingIntent: a recorded-but-unfinalized intent (a
// clean publish whose finalize never ran) drains to exactly one branch,
// one PR, one recorded outcome, and a dispatched row — with no second
// external effect.
func TestDrainConvergesPendingIntent(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	cand := testCandidate(t)

	// Publish records the intent and creates the branch + PR, but nothing
	// finalizes it: the outbox row is left pending, as after a crash
	// between the external effect and local acceptance.
	if _, err := h.pub.Publish(ctx, cand, testApprovedRecipes()); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Fatalf("pending after seed publish = %d, want 1", got)
	}

	n, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, cand))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("drain finalized %d, want 1", n)
	}

	if got := len(h.gh.refs); got != 1 {
		t.Errorf("branches = %d, want 1", got)
	}
	createPR := http.MethodPost + " " + testRepoPath + "/pulls"
	if got := countRequests(h.gh.requestLog(), createPR); got != 1 {
		t.Errorf("PR creations = %d, want exactly 1 (the drain converged, not re-created)", got)
	}
	if got := len(pendingPublications(t, h.store)); got != 0 {
		t.Errorf("pending after drain = %d, want 0 (dispatched)", got)
	}

	id := testCandidateIdentity(t)
	assertOutcomeRecorded(t, h.store, publish.OutcomeKey(id), publish.Outcome{
		Identity:         id.Digest(),
		Repo:             "freeside-ai/evidence-repo",
		BaseRef:          "main",
		HeadSHA:          testHeadSHA,
		Branch:           id.BranchName(),
		PRNumber:         101,
		EvidenceEligible: true,
	})
}

func TestDrainRecoversHeadTransportBeforeForgeConvergence(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	cand := testCandidate(t)
	id := testCandidateIdentity(t)
	key, err := publish.IntentKey(cand.InvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := publish.Intent{
		Identity: id.Digest(), InvocationID: cand.InvocationID,
		Repo: cand.Repo, BaseRef: cand.BaseRef, SourceHeadSHA: cand.HeadSHA,
		AuthorizationID: *cand.AuthorizationID,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(
		ctx, key, publish.IntentKindPublication, payload,
		nil,
	); err != nil {
		t.Fatalf("record intent: %v", err)
	}

	transportCalls := 0
	resolver := resolverFor(t, cand)
	resolver.publishHead = func(_ context.Context, input publish.IdentityInput) error {
		transportCalls++
		identity, err := publish.DeriveIdentity(input)
		if err != nil {
			return err
		}
		h.gh.mu.Lock()
		h.gh.refs[identity.BranchName()] = cand.HeadSHA
		h.gh.mu.Unlock()
		return nil
	}
	if n, err := publish.DrainPendingPublications(
		ctx, h.store, h.pub, resolver,
	); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v, want 1, nil", n, err)
	}
	if transportCalls != 1 {
		t.Fatalf("recovered transport calls = %d, want 1", transportCalls)
	}
	if refs, prs := len(h.gh.refs), len(h.gh.prs); refs != 1 || prs != 1 {
		t.Fatalf("recovered forge resources = refs:%d prs:%d, want 1 each", refs, prs)
	}
}

func TestPublishAfterGateAndFinalizeDoesNotRegateReturnedResult(t *testing.T) {
	ctx := context.Background()
	audit := testWorkflowAudit(t)
	s := newTestStore(t)
	seedDecisionRecords(t, s)
	gh := newFakeGitHub(t)
	pub := storeBackedPublisher(t, s, gh, mutableWorkflowAuditor{audit: &audit})
	cand := testCandidate(t)

	result, err := pub.PublishAfterGateAndFinalize(
		ctx,
		cand,
		testApprovedRecipes(),
		func(_ context.Context, input publish.IdentityInput) error {
			identity, err := publish.DeriveIdentity(input)
			if err != nil {
				return err
			}
			gh.mu.Lock()
			gh.refs[identity.BranchName()] = cand.HeadSHA
			gh.mu.Unlock()
			// The external head now exists. A second pre-effect audit would
			// reject this drift and strand the returned PR result.
			audit.OIDCAvailable = true
			return nil
		},
	)
	if err != nil {
		t.Fatalf("publish and finalize returned result: %v", err)
	}
	if result.PRNumber != 101 {
		t.Fatalf("result = %+v, want PR 101", result)
	}
	if got := len(pendingPublications(t, s)); got != 0 {
		t.Fatalf("pending after direct finalize = %d, want 0", got)
	}
	id := testCandidateIdentity(t)
	assertOutcomeRecorded(t, s, publish.OutcomeKey(id), publish.Outcome{
		Identity:         id.Digest(),
		Repo:             cand.Repo,
		BaseRef:          cand.BaseRef,
		HeadSHA:          cand.HeadSHA,
		Branch:           id.BranchName(),
		PRNumber:         101,
		EvidenceEligible: true,
	})
}

// TestDrainNoPending: an empty outbox drains to zero with no GitHub
// traffic.
func TestDrainNoPending(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)

	n, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, testCandidate(t)))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 0 {
		t.Errorf("drain finalized %d, want 0", n)
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0", got)
	}
}

func TestDrainPublicationIntentScopesRecoveryToInvocation(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	first := testCandidate(t)
	second := first
	second.InvocationID = "inv-0002"

	if _, err := h.pub.Publish(ctx, first, testApprovedRecipes()); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if _, err := h.pub.Publish(ctx, second, testApprovedRecipes()); err != nil {
		t.Fatalf("publish second: %v", err)
	}
	if got := len(pendingPublications(t, h.store)); got != 2 {
		t.Fatalf("pending before scoped drain = %d, want 2", got)
	}

	n, err := publish.DrainPublicationIntent(
		ctx, h.store, h.pub, resolverFor(t, second), second.InvocationID,
	)
	if err != nil {
		t.Fatalf("scoped drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("scoped drain finalized %d, want 1", n)
	}
	pending := pendingPublications(t, h.store)
	firstKey, err := publish.IntentKey(first.InvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].IdempotencyKey != firstKey {
		t.Fatalf("pending after scoped drain = %+v, want only %q", pending, firstKey)
	}
}

// TestDrainRejectsCorruptIntent: a pending row whose payload names a
// different invocation than its idempotency key is never published and
// never dispatched; it stays pending as loud evidence (mirrors signet's
// TestDispatchRejectsCorruptIntent).
func TestDrainRejectsCorruptIntent(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)

	corruptKey := "publish/inv-corrupt/" + publish.IntentKindPublication
	payload, err := publish.Intent{
		Identity:        testCandidateIdentity(t).Digest(),
		InvocationID:    "inv-other", // disagrees with the key
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		AuthorizationID: testCandidateAuthorization(t).ID,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, corruptKey, publish.IntentKindPublication, payload)
		return err
	}); err != nil {
		t.Fatalf("seed corrupt intent: %v", err)
	}

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, testCandidate(t))); err == nil {
		t.Fatal("corrupt intent drained, want error")
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (corrupt intent never reached GitHub)", got)
	}
	pending := pendingPublications(t, h.store)
	if len(pending) != 1 || pending[0].IdempotencyKey != corruptKey {
		t.Errorf("pending = %v, want the corrupt row still pending", pending)
	}
}

// TestDrainRejectsDivergedResolver: a resolver that returns a candidate
// deriving a different identity than the committed intent is refused
// before any external effect — zero branches, zero PRs, row stays
// pending. This is the returned-object trust boundary: the drain trusts
// the engine to reload a candidate, never to reload the wrong one.
func TestDrainRejectsDivergedResolver(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)

	// Seed a self-consistent intent for the real candidate directly on
	// the ledger (no external effect yet).
	id := testCandidateIdentity(t)
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := publish.Intent{
		Identity:        id.Digest(),
		InvocationID:    "inv-0001",
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		AuthorizationID: testCandidateAuthorization(t).ID,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(ctx, key, publish.IntentKindPublication, intentPayload, nil); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// The resolver returns a divergent candidate: a different head yields
	// a different identity.
	recipe := testRecipe
	diverged := publish.Candidate{
		Repo:         "freeside-ai/evidence-repo",
		BaseRef:      "main",
		HeadSHA:      testOtherSHA,
		Title:        "Diverged candidate",
		Body:         "Different content.",
		Artifacts:    []domain.Artifact{testArtifact(t, testOtherSHA)},
		RecipeDigest: &recipe,
		InvocationID: "inv-0001",
	}

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, fakeResolver{cand: diverged, approved: testApprovedRecipes()}); err == nil {
		t.Fatal("diverged resolver drained, want error")
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (refused before any effect)", got)
	}
	if got := len(h.gh.refs); got != 0 {
		t.Errorf("branches = %d, want 0", got)
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Errorf("pending = %d, want 1 (intent stays pending)", got)
	}
}

// TestDrainRejectsInvocationMismatch: a resolver that returns the right
// content (same identity) but a different InvocationID is refused before
// any external effect. The content identity excludes the invocation, so
// the content-axis check alone would pass; without the attempt-axis check
// Publish would record a SECOND outbox row under the resolver's key,
// leaving the original intent to re-drive forever. Zero effect, exactly
// one pending row (the original), no second intent recorded.
func TestDrainRejectsInvocationMismatch(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)

	id := testCandidateIdentity(t)
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := publish.Intent{
		Identity:        id.Digest(),
		InvocationID:    "inv-0001",
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		AuthorizationID: testCandidateAuthorization(t).ID,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(ctx, key, publish.IntentKindPublication, intentPayload, nil); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// Same content (same identity), different invocation.
	mismatched := testCandidate(t)
	mismatched.InvocationID = "inv-other"

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, fakeResolver{cand: mismatched, approved: testApprovedRecipes()}); err == nil {
		t.Fatal("invocation mismatch drained, want error")
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (refused before any effect)", got)
	}
	pending := pendingPublications(t, h.store)
	if len(pending) != 1 || pending[0].IdempotencyKey != key {
		t.Errorf("pending = %v, want only the original intent (no second row under the resolver's invocation)", pending)
	}
}

// TestDrainRejectsAuthorizationMismatch (#168): a resolver that returns the
// right content (same identity) and the same invocation, but a candidate
// bound to a different authorization than the intent committed, is refused
// before any external effect. The identity excludes the authorization
// binding, so the content and attempt axes both pass; without the
// authorization-axis check recovery would silently retarget to a different
// authorizing record. Zero effect, the original row stays pending.
func TestDrainRejectsAuthorizationMismatch(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)

	id := testCandidateIdentity(t)
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := publish.Intent{
		Identity:        id.Digest(),
		InvocationID:    "inv-0001",
		Repo:            "freeside-ai/evidence-repo",
		BaseRef:         "main",
		SourceHeadSHA:   testHeadSHA,
		AuthorizationID: testCandidateAuthorization(t).ID,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(ctx, key, publish.IntentKindPublication, intentPayload, nil); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// A distinct authorizing record for the same candidate (a re-run under a
	// different producing invocation mints a different content id). Same
	// identity and publishing invocation, different authorization.
	otherIn := authorizingInput(t)
	otherIn.InvocationID = "inv-verify-other"
	otherID := newAuthorization(t, otherIn).ID
	retargeted := testCandidate(t)
	retargeted.AuthorizationID = &otherID

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, fakeResolver{cand: retargeted, approved: testApprovedRecipes()}); err == nil {
		t.Fatal("authorization mismatch drained, want error")
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (refused before any effect)", got)
	}
	pending := pendingPublications(t, h.store)
	if len(pending) != 1 || pending[0].IdempotencyKey != key {
		t.Errorf("pending = %v, want the original intent still pending", pending)
	}
}

// TestDrainRefusesForeignOutcomeRow (refute-first: returned-inbox-row
// trust boundary): the inbox is unique by idempotency key alone, so if
// the outcome key is already occupied by a different record, the finalize
// must not mark the intent dispatched with no valid outcome recorded. It
// fails closed and the intent stays pending (the finalize transaction
// rolls back the mark too).
func TestDrainRefusesForeignOutcomeRow(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	cand := testCandidate(t)

	// A pending intent whose branch and PR already exist (publish left it
	// unfinalized), so the drain reaches the finalize.
	if _, err := h.pub.Publish(ctx, cand, testApprovedRecipes()); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	// Pre-occupy the outcome key with a foreign record.
	id := testCandidateIdentity(t)
	if err := h.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(id), publish.IntentKindOutcome, []byte(`{"foreign":true}`))
		return err
	}); err != nil {
		t.Fatalf("seed foreign outcome: %v", err)
	}

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, cand)); err == nil {
		t.Fatal("drain finalized over a foreign outcome row, want error")
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Errorf("pending = %d, want 1 (finalize rolled back, intent not dispatched)", got)
	}
}

// TestDrainRefusesForeignOutcomeKind widens the returned-row refute pass:
// matching payload bytes are not enough when the inbox is unique by key
// alone. A row of another kind under the outcome key must roll back the
// dispatched mark and remain loud, even if it copied the expected payload.
func TestDrainRefusesForeignOutcomeKind(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	cand := testCandidate(t)

	res, err := h.pub.Publish(ctx, cand, testApprovedRecipes())
	if err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	outcome := publish.Outcome{
		Identity:         res.Identity.Digest(),
		Repo:             cand.Repo,
		BaseRef:          cand.BaseRef,
		HeadSHA:          cand.HeadSHA,
		Branch:           res.Branch,
		PRNumber:         res.PRNumber,
		EvidenceEligible: true,
	}
	payload, err := outcome.Encode()
	if err != nil {
		t.Fatalf("encode outcome: %v", err)
	}
	if err := h.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(res.Identity), "foreign.kind", payload)
		return err
	}); err != nil {
		t.Fatalf("seed foreign outcome kind: %v", err)
	}

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, cand)); err == nil {
		t.Fatal("drain finalized over a foreign-kind outcome row, want error")
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Errorf("pending = %d, want 1 (finalize rolled back, intent not dispatched)", got)
	}
}

// TestPublishIdentityMatchesDerivation pins the drain's pre-publish
// identity derivation against Publisher.Publish: a clean publish's
// Result.Identity must equal the identity derived from the same
// candidate coordinates, so the drain's divergence guard (which derives
// the same way) can never disagree with a real publish.
func TestPublishIdentityMatchesDerivation(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	res, err := h.pub.Publish(ctx, testCandidate(t), testApprovedRecipes())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Identity.Digest() != testCandidateIdentity(t).Digest() {
		t.Errorf("Result.Identity = %s, want the derived identity %s", res.Identity.Digest(), testCandidateIdentity(t).Digest())
	}
}
