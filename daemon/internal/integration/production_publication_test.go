package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type productionRoom struct {
	recipe []byte
	read   func(context.Context) ([]byte, error)
	reads  int
	runs   int
	fail   bool
}

func (r *productionRoom) ReadRecipe(ctx context.Context) ([]byte, error) {
	r.reads++
	if r.read != nil {
		return r.read(ctx)
	}
	return bytes.Clone(r.recipe), nil
}

func (r *productionRoom) Run(
	_ context.Context, _ string, _ []string,
) (verify.StepResult, error) {
	r.runs++
	if r.fail {
		return verify.StepResult{ExitCode: 1, Output: []byte("injected verification failure\n")}, nil
	}
	return verify.StepResult{}, nil
}

type productionCrashSeams struct {
	reviewRecovery    func(context.Context) error
	afterVerification func() error
	afterPublication  func() error
	afterReady        func() error
	afterBlocked      func() error
	afterTerminal     func() error
	afterLockRelease  func() error
}

type faultReviewSource struct {
	exec.ReviewSource
	requestCalls          int
	inspectCalls          int
	failInspectAt         int
	pollCalls             int
	failPollAt            int
	failPollWith          error
	verifyCalls           int
	failVerifyAt          int
	failVerifyWith        error
	failAuthorityWith     error
	failAuthorityAt       int
	authorityCalls        int
	failSupersessionWith  error
	failSupersessionAt    int
	supersessionCalls     int
	failRequestAfterStart bool
	failRequestWith       error
	requestedWorkspace    string
}

func (s *faultReviewSource) RequestReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	s.requestCalls++
	// failRequestWith models a pre-start launch failure (e.g. a workspace
	// preparation conformance contradiction): the invocation never starts, so
	// the wrapped source is left untouched.
	if s.failRequestWith != nil {
		return s.failRequestWith
	}
	if err := s.ReviewSource.RequestReview(ctx, id, req); err != nil {
		return err
	}
	s.requestedWorkspace = req.Workspace
	if s.failRequestAfterStart {
		s.failRequestAfterStart = false
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   errors.New("injected transient review preparation failure"),
		}
	}
	return nil
}

func (s *faultReviewSource) Inspect(
	ctx context.Context, id domain.InvocationID,
) (exec.Status, error) {
	s.inspectCalls++
	if s.inspectCalls == s.failInspectAt {
		return "", &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   errors.New("injected transient review inspection failure"),
		}
	}
	return s.ReviewSource.Inspect(ctx, id)
}

func (s *faultReviewSource) Poll(
	ctx context.Context, id domain.InvocationID,
) (exec.ReviewResult, error) {
	s.pollCalls++
	if s.pollCalls == s.failPollAt {
		return exec.ReviewResult{}, s.failPollWith
	}
	return s.ReviewSource.Poll(ctx, id)
}

func (s *faultReviewSource) Verify(
	ctx context.Context, id domain.InvocationID, expectedBase, expectedHead string,
) error {
	s.verifyCalls++
	if s.verifyCalls == s.failVerifyAt {
		if s.failVerifyWith != nil {
			return s.failVerifyWith
		}
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   errors.New("injected transient review verification failure"),
		}
	}
	return s.ReviewSource.Verify(ctx, id, expectedBase, expectedHead)
}

func (s *faultReviewSource) VerifyRequestAuthority(
	ctx context.Context, id domain.InvocationID, expected domain.Digest,
) error {
	s.authorityCalls++
	if s.failAuthorityWith != nil &&
		(s.failAuthorityAt == 0 || s.authorityCalls == s.failAuthorityAt) {
		return s.failAuthorityWith
	}
	verifier, ok := s.ReviewSource.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		return errors.New("wrapped review source does not verify request authority")
	}
	return verifier.VerifyRequestAuthority(ctx, id, expected)
}

func (s *faultReviewSource) VerifyReviewRequestSupersession(
	ctx context.Context, id domain.InvocationID, expected exec.ReviewRequest,
) error {
	s.supersessionCalls++
	if s.failSupersessionWith != nil &&
		(s.failSupersessionAt == 0 || s.supersessionCalls == s.failSupersessionAt) {
		return s.failSupersessionWith
	}
	if verifier, ok := s.ReviewSource.(exec.ReviewRequestSupersessionVerifier); ok {
		return verifier.VerifyReviewRequestSupersession(ctx, id, expected)
	}
	return nil
}

type productionPublicationHarness struct {
	*publicationHarness
	runID                     domain.RunID
	projectID                 domain.ProjectID
	image                     domain.ProjectImage
	driver                    *fake.StageDriver
	replay                    engine.ProductionReplay
	room                      *productionRoom
	reviewer                  *fake.ReviewSource
	reviewSource              exec.ReviewSource
	reviewConfigurationDigest domain.Digest
	workflow                  *engine.Engine
	invocation                domain.InvocationID
	recipeReadTimeout         time.Duration
	declaration               *domain.WorkUnitDeclaration
}

func newProductionPublicationHarness(t *testing.T, resultHead string) *productionPublicationHarness {
	return newProductionPublicationHarnessWithPolicyKeys(t, resultHead, nil)
}

func newProductionPublicationHarnessWithBoundIssue(
	t *testing.T, resultHead string, boundIssue int,
) *productionPublicationHarness {
	return newProductionPublicationHarnessWithPolicyKeysAndBoundIssue(
		t, resultHead, nil, &boundIssue,
	)
}

func newProductionPublicationHarnessWithPolicyKeys(
	t *testing.T, resultHead string, extraKeys []domain.PolicyKey,
) *productionPublicationHarness {
	return newProductionPublicationHarnessWithPolicyKeysAndBoundIssue(
		t, resultHead, extraKeys, nil,
	)
}

func newProductionPublicationHarnessWithPolicyKeysAndBoundIssue(
	t *testing.T,
	resultHead string,
	extraKeys []domain.PolicyKey,
	boundIssue *int,
) *productionPublicationHarness {
	t.Helper()
	h := newPublicationHarness(t)
	// Production verification reads the onboarding-bound external recipe
	// embedded in the project image. The managed repository intentionally has
	// no in-tree .freeside/verify.json.

	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: fakePublicationRepo, RepositoryID: h.profile.RepositoryID,
		CommitSHA: h.baseSHA, RecipeDigest: h.recipeD,
		PreparationCommand: []string{"/usr/bin/true"},
		BaseImageRef:       domain.ImageRef("ghcr.io/freeside-ai/base@sha256:" + strings.Repeat("1", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/freeside-ai/project@sha256:" + strings.Repeat("2", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.WriteInternal(h.ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(h.ctx, testIdentity, fakePublicationTime); err != nil {
			return err
		}
		if err := tx.RecordProjectImage(h.ctx, image); err != nil {
			return err
		}
		backend := productionPublicationBackend(t)
		conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
			Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
			Outcome: domain.ConformancePassed, ConfigurationDigest: backend.ConfigurationDigest(),
			Capabilities: conformantCeiling(t), ProvedAt: fakePublicationTime,
		})
		if err != nil {
			return err
		}
		_, err = tx.RecordBackendConformance(h.ctx, conformance)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	runID := domain.RunID("run-production-publication")
	projectID := domain.ProjectID("project-production-publication")
	policyKeys := append([]domain.PolicyKey{{
		Key: "paths", Value: "README.md",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest(string(runID), "policy-source"),
		},
	}}, extraKeys...)
	spec, policy, resolved := registerSubmissionArtifactsWithPolicyKeys(
		t, h.store, string(runID), policyKeys,
	)
	specBody := submissionSpecification(string(runID))
	putProductionBlob(t, h, spec.Digest, specBody)
	submitted, err := engine.SubmitProductionRun(h.ctx, h.store, engine.ProductionRunSpec{
		RunID: runID, ProjectID: projectID, SpecArtifactID: spec.ID,
		PolicyArtifactID: policy.ID, ResolvedPolicy: resolved,
		Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var declaration *domain.WorkUnitDeclaration
	if boundIssue != nil {
		captured, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
			BoundIssue:          boundIssue,
			DeclaredPaths:       []string{"README.md"},
		}, runID, projectID, fakePublicationTime)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.WriteInternal(h.ctx, func(tx *store.InternalTx) error {
			return tx.RecordWorkUnitDeclaration(h.ctx, captured)
		}); err != nil {
			t.Fatal(err)
		}
		declaration = &captured
	}
	replay := buildProductionReplay(t, h, runID, spec.Digest, specBody, boundIssue)
	if resultHead == "" {
		resultHead = replay.HeadSHA
	}
	driver, err := fake.NewStageDriverAt(filepath.Join(h.workDir, "production-driver"))
	if err != nil {
		t.Fatal(err)
	}
	driver.Script(submitted.InvocationID, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{HeadSHA: resultHead, Summary: "Claude export completed."},
	})
	p := &productionPublicationHarness{
		publicationHarness: h, runID: runID, projectID: projectID,
		image: image, driver: driver, replay: replay,
		room:                      &productionRoom{recipe: bytes.Clone(h.recipe)},
		reviewer:                  fake.NewReviewSource(),
		reviewConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		invocation:                submitted.InvocationID, declaration: declaration,
	}
	p.reviewSource = p.reviewer
	// Mirror production: the signet decision-time adoption gate reads the
	// engine's effective reviewer configuration. The closure reads the
	// harness field so tests that drift the digest after construction stay
	// in lockstep with the engines they rebuild.
	p.attention = signet.NewService(p.store, signet.WithBlobStore(p.blobs),
		signet.WithEffectiveReviewConfiguration(func() domain.Digest {
			return p.reviewConfigurationDigest
		}))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return p
}

func productionPublicationBackend(t *testing.T) fake.RunnerBackend {
	t.Helper()
	return fake.RunnerBackend{
		BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Caps:        exec.NewCapabilitySet(conformantCeiling(t)...),
	}
}

func buildProductionReplay(
	t *testing.T,
	h *publicationHarness,
	runID domain.RunID,
	specDigest domain.Digest,
	specBody []byte,
	boundIssue *int,
) engine.ProductionReplay {
	t.Helper()
	workspace := t.TempDir()
	writeFile(t, workspace, "README.md", "production change\n")
	handoff := filepath.Join(t.TempDir(), "handoff")
	manifest, err := export.Export(os.DirFS(workspace), handoff, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := productionDigest(manifestBody)
	putProductionBlob(t, h, manifestDigest, manifestBody)
	for _, entry := range manifest.Entries {
		if entry.Kind != export.EntryRegular || entry.Digest == nil {
			continue
		}
		digest := domain.Digest(*entry.Digest)
		hexDigits := strings.TrimPrefix(string(digest), "sha256:")
		body, err := os.ReadFile(filepath.Join(handoff, "blobs", "sha256", hexDigits)) //nolint:gosec // test-owned export path and digest
		if err != nil {
			t.Fatal(err)
		}
		putProductionBlob(t, h, digest, body)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, h.baseDir, "clone", "-q", "--no-hardlinks", ".", checkout)
	policy, err := (importer.Policy{Allowlist: []string{"README.md"}}).WithProtectedPaths(h.profile)
	if err != nil {
		t.Fatal(err)
	}
	options := importer.Options{
		BaseSHA: h.baseSHA, CommitDate: fakePublicationTime,
		AuthorName:  productionPublicationMetadata().CommitAuthor.Name(),
		AuthorEmail: productionPublicationMetadata().CommitAuthor.Email(),
		Policy:      policy,
	}
	options.CommitMessage = engine.FallbackCommitMessage(engine.FallbackCommitMessageInput{
		Spec: specBody, BoundIssue: boundIssue, RunID: runID,
		SpecDigest: specDigest, Policy: policy,
	})
	imported, err := importer.Import(t.Context(), handoff, checkout, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Findings) != 0 || imported.CommitSHA == "" {
		t.Fatalf("production replay import = %#v", imported)
	}
	author := strings.TrimSpace(runGit(
		t, checkout, "show", "-s", "--format=%an <%ae>", imported.CommitSHA,
	))
	wantAuthor := productionPublicationMetadata().CommitAuthor
	if author != wantAuthor.Name()+" <"+wantAuthor.Email()+">" {
		t.Fatalf("production commit author = %q, want App bot attribution", author)
	}
	return engine.ProductionReplay{
		InvocationID:    "inv-implement-run-production-publication",
		ObservedBaseSHA: h.baseSHA, HeadSHA: imported.CommitSHA,
		Manifest: manifest, ManifestDigest: manifestDigest, ImportOptions: options,
	}
}

func productionDigest(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func putProductionBlob(t *testing.T, h *publicationHarness, digest domain.Digest, body []byte) {
	t.Helper()
	if _, err := h.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func (p *productionPublicationHarness) newPublisher(t *testing.T) *publish.Publisher {
	t.Helper()
	ledger, err := publish.NewStoreLedger(p.store)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := publish.NewStoreTrustSource(p.store)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := publish.NewStoreAuthorizationSource(p.store)
	if err != nil {
		t.Fatal(err)
	}
	tokens := p.tokens
	if tokens == nil {
		tokens = integrationTokenSource{}
	}
	return publish.NewPublisher(
		tokens, p.server.Client(), p.server.URL,
		integrationAuditor{audit: p.audit}, ledger, trust, authorizations,
	)
}

// reconcileLanes makes one pass over both loops a daemon runs for this engine:
// the reconcile loop (engine.Run) and the production publication loop
// (engine.RunProductionPublications, issue #425). The lanes are independent,
// so the combined result is their sum; a failing reconcile pass returns before
// the publication pass, matching the loop that stops on a loud error.
func (p *productionPublicationHarness) reconcileLanes() (engine.ReconcileResult, error) {
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil {
		return result, err
	}
	publication, err := p.workflow.ReconcileProductionPublications(p.ctx)
	result.ResultsAccepted += publication.ResultsAccepted
	result.PublicationTasksCompleted += publication.PublicationTasksCompleted
	result.ReadyCleanItemsCreated += publication.ReadyCleanItemsCreated
	result.ReadyDegradedItemsCreated += publication.ReadyDegradedItemsCreated
	result.ReadyItemsCreated += publication.ReadyItemsCreated
	result.BlockedItemsCreated += publication.BlockedItemsCreated
	if publication.LastPRNumber > 0 {
		result.LastPRNumber = publication.LastPRNumber
	}
	return result, err
}

func (p *productionPublicationHarness) newEngine(
	t *testing.T, seams productionCrashSeams, withPublication bool,
) *engine.Engine {
	t.Helper()
	return p.newEngineWithApprovedRecipes(
		t, seams, withPublication, map[domain.Digest]bool{p.recipeD: true},
	)
}

func (p *productionPublicationHarness) newEngineWithApprovedRecipes(
	t *testing.T,
	seams productionCrashSeams,
	withPublication bool,
	approvedRecipes map[domain.Digest]bool,
) *engine.Engine {
	t.Helper()
	return p.newEngineForMode(
		t, seams, withPublication, approvedRecipes, domain.ModeUnattended, false,
	)
}

// reopenStoreWithApprovedRecipes closes and reopens the harness store with a
// new recipe-approval map, the realistic restart shape in which the store's
// boundary policy, not only the engine's, reflects a revoked recipe. It
// mirrors the initial open options so every other boundary policy is unchanged,
// and must run before the next engine is built because engine.New binds the
// store it is given.
func (p *productionPublicationHarness) reopenStoreWithApprovedRecipes(
	t *testing.T, approvedRecipes map[domain.Digest]bool,
) {
	t.Helper()
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(p.ctx, p.dbPath, store.Options{
		ApprovedRecipes: approvedRecipes,
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
			domain.ModeUnattended:  {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context, store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	p.store = reopened
	// The attention service and every later engine wrap the store handle, so
	// rebind the service to the reopened store; engines are rebuilt by callers.
	p.attention = signet.NewService(p.store, signet.WithBlobStore(p.blobs))
}

func (p *productionPublicationHarness) newEngineForMode(
	t *testing.T,
	seams productionCrashSeams,
	withPublication bool,
	approvedRecipes map[domain.Digest]bool,
	mode domain.OperatingMode,
	holdOnly bool,
) *engine.Engine {
	t.Helper()
	identity := testIdentity.ID
	environment := engine.AdmissionEnvironment{
		OperatingMode:  mode,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly, ImageRef: p.image.ImageRef,
		PromptPackageDigest: productionDigest([]byte("prompt package")),
		VendorInstructions: engine.VendorInstructionConfig{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			HostPath: "/nonexistent/production-publication-claude-md",
		},
		AuthIdentityID: &identity,
	}
	options := []engine.Option{
		engine.WithAdmission(
			productionPublicationBackend(t), nil,
			environment, func() time.Time { return fakePublicationTime },
		),
		engine.WithAdmissionDerivation(func(
			_ context.Context, _ domain.InvocationID,
		) (string, domain.BaseRevision, error) {
			return "freeside-handoff-production-publication-ws", domain.BaseRevision{
				Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID,
				BaseRef: "main", BaseSHA: p.baseSHA,
			}, nil
		}),
	}
	if withPublication {
		reviewRecovery := seams.reviewRecovery
		if reviewRecovery == nil {
			reviewRecovery = func(context.Context) error { return nil }
		}
		reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
		p.reviewer.Script(reviewID, fake.ReviewScript{
			Outcome: fake.OutcomeComplete,
			Result: exec.ReviewResult{
				BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
				Provider: "openai", ModelConfiguration: "codex/test",
				CostOwner: "test", CompletedAt: p.now.UTC(),
				CompletionEvidence: productionDigest([]byte("clean review")),
			},
		})
		options = append(options, engine.WithProductionPublication(engine.ProductionPublicationConfig{
			WorkDir:   filepath.Join(p.workDir, "production-publication"),
			Transport: p.transport, Publisher: p.newPublisher(t), Artifacts: p.blobs,
			ApprovedRecipes:           approvedRecipes,
			HoldOnly:                  holdOnly,
			RecipeReadTimeout:         p.recipeReadTimeout,
			HoldRetryInterval:         time.Minute,
			Now:                       func() time.Time { return p.now },
			ReviewSource:              p.reviewSource,
			ReviewRecovery:            reviewRecovery,
			ReviewConfigurationDigest: p.reviewConfigurationDigest,
			NewRoom: func(image domain.ProjectImage) (engine.ProductionVerificationRoom, error) {
				if image.ID != p.image.ID {
					return nil, domain.ErrParentKeyMismatch
				}
				return p.room, nil
			},
			AfterVerification: seams.afterVerification,
			AfterPublication:  seams.afterPublication,
			AfterReady:        seams.afterReady, AfterBlocked: seams.afterBlocked,
			AfterTerminal:        seams.afterTerminal,
			AfterTaskLockRelease: seams.afterLockRelease,
		}))
	}
	workflow, err := engine.New(p.store, p.attention, p.driver, options...)
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

func (p *productionPublicationHarness) startAndRecordExport(t *testing.T) {
	t.Helper()
	record := p.startExecutionExport(t, p.replay.HeadSHA)
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err != nil {
		t.Fatal(err)
	}
}

func (p *productionPublicationHarness) startExecutionExport(
	t *testing.T,
	headSHA string,
) domain.ExecutionExport {
	t.Helper()
	// Dispatch is reconcile-loop work, and no publication task exists yet, so
	// this deliberately stays on the reconcile pass: a harness engine composed
	// without the publication lane must still be able to start its invocation.
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil {
		t.Fatalf("start production invocation: %v", err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("start result = %#v", result)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: p.invocation, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: headSHA,
		ManifestDigest: p.replay.ManifestDigest, RecordedAt: fakePublicationTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (p *productionPublicationHarness) assertReady(t *testing.T) {
	p.assertReadyWithEvidence(t, 2)
}

func (p *productionPublicationHarness) assertReadyWithEvidence(t *testing.T, wantEvidence int) {
	t.Helper()
	var ready domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		ready, err = tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ready.Type != domain.AttentionReadyForFinalReview || ready.PRHeadSHA != p.replay.HeadSHA ||
		len(ready.EvidenceSnapshot) != wantEvidence {
		t.Fatalf("ready item = %#v", ready)
	}
	refs, prs := p.forge.counts()
	if refs != 1 || prs != 1 {
		t.Fatalf("forge effects = %d refs, %d PRs, want one each", refs, prs)
	}
	pullRequests := p.forge.pullRequests()
	metadata := productionPublicationMetadata()
	if len(pullRequests) != 1 || pullRequests[0].Title != metadata.Title ||
		!strings.HasPrefix(pullRequests[0].Body, strings.TrimRight(metadata.Body, "\n")+"\n\n") {
		t.Fatalf("published PR metadata = %#v, want operator-authored title/body", pullRequests)
	}
	var terminal store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		terminal, err = tx.GetInbox(p.ctx, string(p.invocation))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if terminal.Kind != productionTerminalKind {
		t.Fatalf("terminal kind = %q", terminal.Kind)
	}
}

func TestProductionExecutionPublishesOnlyAfterCleanVerification(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultsAccepted != 1 || result.ReadyItemsCreated != 1 ||
		result.ReadyCleanItemsCreated != 1 || result.ReadyDegradedItemsCreated != 0 ||
		result.PublicationTasksCompleted != 1 || result.LastPRNumber == 0 {
		t.Fatalf("publication result = %#v", result)
	}
	p.assertReady(t)
	if p.room.runs != 1 {
		t.Fatalf("verification commands = %d, want 1", p.room.runs)
	}
	if p.room.reads != 1 {
		t.Fatalf("project-image recipe reads = %d, want 1", p.room.reads)
	}
	message := p.transport.pushedCommitMessage()
	wantSubject := "Publish run-production-publication"
	if subject, _, _ := strings.Cut(message, "\n"); subject != wantSubject {
		t.Fatalf("fallback commit subject = %q, want %q", subject, wantSubject)
	}
	if !strings.Contains(message, "Run ID: run-production-publication.") ||
		!strings.Contains(message, "Specification digest:") {
		t.Fatalf("fallback commit message lacks trace facts:\n%s", message)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Outcome != domain.ReviewClean || review.BaseSHA != p.baseSHA ||
			review.HeadSHA != p.replay.HeadSHA || review.Provider != "openai" ||
			review.ModelConfiguration == "" || review.CostOwner == "" ||
			review.ConfigurationDigest != fake.DefaultReviewConfigurationDigest ||
			review.InstructionDigest == "" ||
			review.CompletionEvidence == "" {
			t.Fatalf("review pass = %#v", review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforePushes := p.transport.pushCount()
	if result, err := p.reconcileLanes(); err != nil ||
		result.ResultsAccepted != 0 || result.PublicationTasksCompleted != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if p.transport.pushCount() != beforePushes {
		t.Fatal("converged replay repeated the publication transport")
	}
}

func TestProductionReviewInvocationIDIsWardSafe(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-" + strings.Repeat("a", 64))
	first := engine.ProductionReviewInvocationID(runID, 1)
	second := engine.ProductionReviewInvocationID(runID, 2)
	if len(first) > 32 || !strings.HasPrefix(string(first), "review-") || first == second {
		t.Fatalf("review invocation ids = %q / %q", first, second)
	}
}

func TestProductionCleanReviewDoesNotSurviveInstructionAuthorityChange(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	old, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 1),
		RunID:        p.runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		InstructionDigest:   productionDigest([]byte("superseded instructions")), CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("prior clean review")), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, old, nil)
	}); err != nil {
		t.Fatal(err)
	}
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test",
			CostOwner: "test", CompletedAt: p.now,
			CompletionEvidence: productionDigest([]byte("replacement clean review")),
		},
	})
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 1 {
		t.Fatalf("instruction re-review result = %#v", result)
	}
	_, current, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		record, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if record.Round != 2 || record.InstructionDigest != current.ResultDigest {
			t.Fatalf("replacement review record = %#v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionFindingsDoNotSurviveInstructionAuthorityChange(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	old, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 1),
		RunID:        p.runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		InstructionDigest:   productionDigest([]byte("superseded instructions")), CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("prior findings review")), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{"old-finding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldFinding := domain.Finding{
		ID: "old-finding", RunID: p.runID, Source: "codex_local", Severity: "P1",
		Location: "daemon/main.go:12", Message: "stale finding", RawText: "stale finding",
		CreatedAt: p.now,
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, old, []domain.Finding{oldFinding})
	}); err != nil {
		t.Fatal(err)
	}
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("replacement clean review")),
		},
	})
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.BlockedItemsCreated != 0 {
		t.Fatalf("stale findings re-review = %#v, %v", result, err)
	}
}

func TestProductionLegacyReviewRequestAdvancesToAuthoritativeRound(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	fault := &faultReviewSource{
		ReviewSource:    p.reviewer,
		failAuthorityAt: 1,
		failAuthorityWith: errors.Join(exec.ErrLegacyReviewRequest,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   errors.New("pre-authority request rejected after teardown"),
			}),
	}
	p.reviewSource = fault
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("legacy request supersession = %#v, %v", result, err)
	}
	p.now = p.now.Add(2 * time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("authoritative replacement")),
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("authoritative replacement review = %#v, %v", result, err)
	}
}

func TestProductionLegacyReviewRequestSupersessionPreflightAdvancesRound(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	fault := &faultReviewSource{
		ReviewSource: p.reviewer, failSupersessionAt: 1,
		failSupersessionWith: errors.Join(exec.ErrLegacyReviewRequest,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   errors.New("legacy request rejected after teardown"),
			}),
	}
	p.reviewSource = fault
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("legacy supersession preflight = %#v, %v", result, err)
	}
	p.now = p.now.Add(2 * time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("legacy preflight replacement")),
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("legacy preflight replacement review = %#v, %v", result, err)
	}
}

func TestProductionSupersededInstructionRequestAdvancesToNewRound(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	fault := &faultReviewSource{
		ReviewSource: p.reviewer, failSupersessionAt: 1,
		failSupersessionWith: &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient, Err: exec.ErrSupersededReviewRequest,
		},
	}
	p.reviewSource = fault
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("superseded instruction request = %#v, %v", result, err)
	}
	p.now = p.now.Add(2 * time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("superseding instruction round")),
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("superseding instruction review = %#v, %v", result, err)
	}
}

func TestProductionPersistedCleanReviewRegatesRequestAuthority(t *testing.T) {
	t.Parallel()
	// A persisted clean review record is replayed by the pre-publication review
	// gate, which re-gates the persisted request authority before treating the
	// record as a pass (issue #527): a rewritten-but-valid row fails closed with
	// no publication effect.
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	_, current, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 1),
		RunID:        p.runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		InstructionDigest:   current.ResultDigest, CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("prior clean review")), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	fault := &faultReviewSource{
		ReviewSource:    p.reviewer,
		failAuthorityAt: 1,
		failAuthorityWith: &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.New("injected persisted instruction closure corruption"),
		},
	}
	p.reviewSource = fault
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err == nil || fault.authorityCalls != fault.failAuthorityAt {
		t.Fatalf("persisted clean review authority re-gate = calls %d, %v",
			fault.authorityCalls, err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("authority re-gate failure published = %d refs, %d prs", refs, prs)
	}
}

func TestProductionReviewConfigurationMustMatchTrustProfile(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 1),
		RunID:        p.runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		InstructionDigest:   productionDigest([]byte("prior instructions")), CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("prior clean review")), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	p.reviewConfigurationDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
	recoveryCalls := 0
	p.workflow = p.newEngine(t, productionCrashSeams{reviewRecovery: func(context.Context) error {
		recoveryCalls++
		if recoveryCalls == 1 {
			return errors.New("old review topology is still cleaning up")
		}
		return nil
	}}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("configuration mismatch bypassed failed startup review recovery")
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err == nil {
			return errors.New("configuration failure recorded before recovery")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	// The configuration failure parks the run (issue #611, revising #527
	// decision 3 for this class): no terminal record, no blocked item, and
	// the recovery-bearing item is raised by the next tick's gate pass.
	if result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("configuration-mismatched result = %#v", result)
	}
	if recoveryCalls != 2 {
		t.Fatalf("startup review recovery calls = %d, want retry before configuration failure", recoveryCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureConfiguration || failure.Round != 2 {
			t.Fatalf("configuration failure = %#v", failure)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("parked configuration replay = %#v, %v", result, err)
	}
	if recoveryCalls != 2 {
		t.Fatalf("startup review recovery repeated after success: %d calls", recoveryCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		digest, err := tx.ReviewFailureBodyDigest(p.ctx, failure.InvocationID)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewConfiguration ||
			!item.Offers(domain.ActionAdoptReviewConfiguration) || item.Status != domain.StatusOpen ||
			item.ReviewConfigurationRecovery == nil ||
			!item.ReviewConfigurationRecovery.Matches(failure, digest) ||
			item.ReviewConfigurationRecovery.SupersededProfileDigest != p.profile.ProfileDigest {
			t.Fatalf("configuration recovery item = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewResultConfigurationMismatchIsContradiction(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("park unapproved-configuration contradiction: %v", err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureContradiction {
			t.Fatalf("configuration contradiction = %#v", failure)
		}
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewContradiction ||
			!item.Offers(domain.ActionRecoverReview) {
			t.Fatalf("configuration contradiction item = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionParkedReviewContradictionPrecedesConfigurationDrift(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("park contradiction: %v", err)
	}

	// A restarted daemon may have a different effective reviewer
	// configuration. The unresolved contradiction remains authoritative and
	// must not be replaced by a new configuration failure before recovery.
	p.reviewConfigurationDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("configuration drift bypassed parked contradiction = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.InvocationID != id || failure.Round != 1 ||
			failure.Class != domain.ReviewFailureContradiction {
			t.Fatalf("latest failure after configuration drift = %#v", failure)
		}
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen || !item.Offers(domain.ActionRecoverReview) {
			t.Fatalf("parked contradiction after configuration drift = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewFindingsEscalateWithoutReady(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(reviewID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review findings")),
			Findings: []domain.Finding{{
				ID: "review-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: "daemon/main.go:12", Message: "unsafe transition", RawText: "unsafe transition",
				CreatedAt: p.now,
			}},
		},
	})
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 1 ||
		result.PublicationTasksCompleted != 1 || result.LastPRNumber != 0 {
		t.Fatalf("findings result = %#v", result)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("findings escalation produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID))); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("ready item after findings: %w", err)
		}
		item, err := tx.GetAttentionItem(p.ctx,
			domain.ItemID(fmt.Sprintf("production-review-%s-1", p.runID)))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewDiminishing || item.PRHeadSHA != p.replay.HeadSHA {
			t.Fatalf("review attention = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionBaseAdvanceAfterReviewBlocksAtPublisher(t *testing.T) {
	t.Parallel()
	// Under pre-publication review (issue #527, decision 1) the §7 review runs
	// against the admitted base, so a base advance discovered at publication is
	// owned by the publisher's exact-base gate, not a review dispute: the
	// candidate reviews clean, then the publisher refuses the moved base and the
	// run blocks with no forge effect.
	p := newProductionPublicationHarness(t, "")
	p.audit.AuditedCommitSHA = strings.Repeat("f", 40)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 1 {
		t.Fatalf("base-advanced result = %#v", result)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("base advance produced forge effects = %d refs, %d prs", refs, prs)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx,
		domain.ItemID("production-publish-blocked-"+string(p.runID)))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Reason != "The target base advanced after admission; rerun and reverify against the current base." {
		t.Fatalf("base-advance block reason = %q", blocked.Item.Reason)
	}
	// The candidate was reviewed clean before publication was attempted.
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Outcome != domain.ReviewClean || review.HeadSHA != p.replay.HeadSHA {
			t.Fatalf("pre-publication review = %#v", review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionCleanReviewIsInvalidatedByPublishedHeadAdvance retired with the
// pre-publication re-anchor (issue #527): no PR head exists during the review
// pass, so a published-head advance has no counterpart in the review gate. That
// class returns as #524's external-response capability over an already-published
// PR, covered there by the #496/#514 active-resource invalidation machinery.

func TestProductionCleanReviewReplayPublishesWithoutNewRound(t *testing.T) {
	t.Parallel()
	// Record replay across a crash between the clean review pass and publication:
	// the review round completes and its record persists, the publishing push
	// then fails transiently, and a later pass replays the persisted record and
	// publishes without invoking a new review round (issue #527).
	p := newProductionPublicationHarness(t, "")
	fault := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = fault
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	p.transport.failNextPush()
	// A transient publishing push backs off into an empty result, not a loud
	// error; the clean review round has already completed and persisted.
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("transient publishing push = %#v, %v", result, err)
	}
	if fault.requestCalls != 1 {
		t.Fatalf("first pass review requests = %d, want 1", fault.requestCalls)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("readiness created before publication: %v", err)
	}
	// The clean review record persisted before the failed push.
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Outcome != domain.ReviewClean || review.Round != 1 {
			t.Fatalf("persisted review before push = %#v", review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Later pass past the transient backoff: the failed-push flag has cleared, so
	// publication succeeds by replaying the record without a new review round.
	p.now = p.now.Add(time.Minute)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 ||
		result.LastPRNumber == 0 {
		t.Fatalf("record-replay publish = %#v, %v", result, err)
	}
	if fault.requestCalls != 1 {
		t.Fatalf("record replay invoked a new review round: requests = %d", fault.requestCalls)
	}
	p.assertReady(t)
}

func TestProductionPublishedWithoutCleanRecordFailsClosed(t *testing.T) {
	t.Parallel()
	// Decision 2: a published run reaches readiness only through a clean,
	// candidate-bound review record. When the latest review state is not one
	// (here, a later record bound to a foreign head, standing in for a run
	// published under the retired post-publication order), the readiness re-gate
	// fails closed rather than deriving silent readiness (issue #527).
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after publication, before readiness")
		},
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("afterPublication seam did not interrupt reconciliation")
	}
	foreign, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 2),
		RunID:        p.runID, Round: 2, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: fake.DefaultReviewConfigurationDigest,
		InstructionDigest:   productionDigest([]byte("foreign instructions")), CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: strings.Repeat("e", 40), CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("foreign candidate review")), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, foreign, nil)
	}); err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	// Decision 2's fail-closed re-gate must be an operator-visible durable
	// hold, not a lane-fatal error: a non-nil reconcile error is exactly what
	// makes runReconcileLoop exit, halting every queued publication and never
	// creating the promised disposition (#527, Codex round 3). So the pass
	// returns nil while holding this one run, matching the sibling recipe
	// re-gate's disposition (Codex round 4 aligned the mechanism).
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatalf("fail-closed re-gate crashed the lane instead of holding: %v", err)
	}
	if result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("fail-closed re-gate outcome = %#v", result)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fail-closed re-gate created readiness: %v", err)
	}
	blocked, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil ||
		!strings.Contains(blocked.Item.Reason, "lacks a clean, candidate-bound review record") {
		t.Fatalf("fail-closed hold item = %#v, %v", blocked, err)
	}
	// The lane keeps running: a follow-up pass returns nil (the loop never
	// exited) and idempotently re-holds without readying the run.
	replay, err := p.reconcileLanes()
	if err != nil || replay.BlockedItemsCreated != 0 || replay.ReadyItemsCreated != 0 {
		t.Fatalf("lane did not survive the hold or was not idempotent = %#v, %v", replay, err)
	}
}

func TestProductionPublishedReviewConfigMustStayProfileApproved(t *testing.T) {
	t.Parallel()
	// Finding 5 (Codex round 4): the readiness recovery re-gate must be no
	// weaker than the pre-publication gate on the trust-profile-approval axis.
	// A published run whose review record matches the daemon's current reviewer
	// configuration, but whose configuration the pinned trust profile no longer
	// approves (profile.Review.ConfigDigest diverged), must fail closed to an
	// operator-visible hold, exactly as the pre-publication gate would. Before
	// the fix, assertReviewedCandidate compared only against the current daemon
	// configuration and would have derived readiness under an unapproved config.
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after publication, before readiness")
		},
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("afterPublication seam did not interrupt reconciliation")
	}
	// A clean, candidate-bound record produced under a drifted reviewer
	// configuration that the daemon now runs but the trust profile never
	// approved (profile.Review.ConfigDigest stays fake.DefaultReviewConfigurationDigest).
	drifted := domain.Digest("sha256:" + strings.Repeat("d", 64))
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: engine.ProductionReviewInvocationID(p.runID, 2),
		RunID:        p.runID, Round: 2, Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: drifted,
		InstructionDigest:   instructions.ResultDigest, CostOwner: "test",
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
		CompletionEvidence: productionDigest([]byte("clean under drifted config")), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(p.ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	// The daemon reviewer configuration drifts to match the record; the pinned
	// trust profile still approves only the original configuration.
	p.reviewConfigurationDigest = drifted
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatalf("profile-unapproved re-gate crashed the lane instead of holding: %v", err)
	}
	if result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 1 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("profile-unapproved config derived readiness or wrong disposition = %#v", result)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("profile-unapproved config created readiness: %v", err)
	}
	blocked, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil ||
		!strings.Contains(blocked.Item.Reason, "trust-approved reviewer configuration") {
		t.Fatalf("profile-unapproved hold item = %#v, %v", blocked, err)
	}
}

func TestProductionPendingReviewPublishesNothing(t *testing.T) {
	t.Parallel()
	// A review still running (StatusPending) keeps the task queued with no
	// publication effect: no branch push, no PR, nothing readied, no record yet
	// (issue #527 acceptance: a pending review blocks publication).
	p := newProductionPublicationHarness(t, "")
	reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(reviewID, fake.ReviewScript{
		PendingInspects: 2, Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("still-running review")),
		},
	})
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 0 || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 || result.LastPRNumber != 0 {
		t.Fatalf("pending review = %#v, %v", result, err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("pending review produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.LatestReviewRecord(p.ctx, p.runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("pending review wrote a record: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionTransientReviewFailureBacksOffAndRetries(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	round1 := engine.ProductionReviewInvocationID(p.runID, 1)
	round2 := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(round1, fake.ReviewScript{Outcome: fake.OutcomeFail})
	p.reviewer.Script(round2, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt:        p.now.Add(2 * time.Second),
			CompletionEvidence: productionDigest([]byte("retry clean")),
		},
	})
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("first review failure = %#v, %v", result, err)
	}
	if _, err := p.reviewer.Inspect(p.ctx, round2); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("round 2 started before backoff: %v", err)
	}
	// Reconstruct the workflow to prove backoff comes from the durable failure
	// timestamp rather than only the process-local retry map.
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("review backoff pass = %#v, %v", result, err)
	}
	p.now = p.now.Add(2 * time.Second)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("review retry = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureTransient || failure.Round != 1 || review.Round != 2 {
			t.Fatalf("review retry records = %#v / %#v", failure, review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionTerminalTransientReviewOutcomeAdvancesRound(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	round1 := engine.ProductionReviewInvocationID(p.runID, 1)
	round2 := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(round1, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt:        p.now,
			CompletionEvidence: productionDigest([]byte("unused terminal result")),
		},
	})
	p.reviewer.Script(round2, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt:        p.now.Add(2 * time.Second),
			CompletionEvidence: productionDigest([]byte("retry clean")),
		},
	})
	faults := &faultReviewSource{
		ReviewSource: p.reviewer, failPollAt: 1,
		failPollWith: errors.Join(exec.ErrNoResult, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient, Err: errors.New("connection reset"),
		}),
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("terminal transient outcome = %#v, %v", result, err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("terminal transient backoff = %#v, %v", result, err)
	}
	p.now = p.now.Add(2 * time.Second)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("terminal transient retry = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureTransient || failure.Round != 1 || review.Round != 2 {
			t.Fatalf("terminal transient records = %#v / %#v", failure, review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewObservationFailuresRetrySameInvocation(t *testing.T) {
	t.Parallel()
	transient := func(message string) error {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient, Err: errors.New(message),
		}
	}
	for _, tc := range []struct {
		name      string
		configure func(*faultReviewSource)
	}{
		{"request preparation", func(source *faultReviewSource) { source.failRequestAfterStart = true }},
		{"post-launch inspect", func(source *faultReviewSource) { source.failInspectAt = 2 }},
		{"poll", func(source *faultReviewSource) {
			source.failPollAt = 1
			source.failPollWith = transient("injected transient review poll failure")
		}},
		{"poll joined not ready", func(source *faultReviewSource) {
			source.failPollAt = 1
			source.failPollWith = errors.Join(exec.ErrResultNotReady,
				transient("injected transient review poll failure"))
		}},
		{"final verification", func(source *faultReviewSource) { source.failVerifyAt = 1 }},
		{"verification joined not ready", func(source *faultReviewSource) {
			source.failVerifyAt = 1
			source.failVerifyWith = errors.Join(exec.ErrResultNotReady,
				transient("injected transient review verification failure"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			faults := &faultReviewSource{ReviewSource: p.reviewer}
			tc.configure(faults)
			p.reviewSource = faults
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.startAndRecordExport(t)
			if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
				t.Fatalf("transient observation = %#v, %v", result, err)
			}
			// The transient did not terminalize the invocation, and it durably
			// recorded the pending same-invocation retry bound to this candidate.
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				if _, err := tx.LatestReviewFailure(p.ctx, p.runID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("transient observation terminalized invocation: %v", err)
				}
				retry, err := tx.GetReviewRetry(p.ctx, p.runID)
				if err != nil {
					return err
				}
				if retry.Round != 1 || retry.BaseSHA != p.baseSHA || retry.HeadSHA != p.replay.HeadSHA ||
					retry.InvocationID != engine.ProductionReviewInvocationID(p.runID, 1) {
					t.Fatalf("persisted review retry = %#v", retry)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Restart inside the backoff. A fresh workflow drops the in-memory
			// deadline, so only the durable row can hold the retry: it must not
			// retry early (no new source calls) or terminalize.
			requestCalls, inspectCalls := faults.requestCalls, faults.inspectCalls
			pollCalls, verifyCalls := faults.pollCalls, faults.verifyCalls
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
				t.Fatalf("review retry after restart before backoff = %#v, %v", result, err)
			}
			if faults.requestCalls != requestCalls || faults.inspectCalls != inspectCalls ||
				faults.pollCalls != pollCalls || faults.verifyCalls != verifyCalls {
				t.Fatalf("review retried early after restart: request=%d/%d inspect=%d/%d poll=%d/%d verify=%d/%d",
					faults.requestCalls, requestCalls, faults.inspectCalls, inspectCalls,
					faults.pollCalls, pollCalls, faults.verifyCalls, verifyCalls)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				_, err := tx.LatestReviewFailure(p.ctx, p.runID)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("restart terminalized the pending retry: %v", err)
			}
			// Advance past the deadline and restart again: the reconstructed
			// deadline has now elapsed, so the same round-1 invocation retries
			// and the run completes. The retry state clears with the success.
			p.now = p.now.Add(time.Second)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			result, err := p.reconcileLanes()
			if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
				t.Fatalf("same-invocation retry after restart = %#v, %v", result, err)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				record, err := tx.LatestReviewRecord(p.ctx, p.runID)
				if err != nil {
					return err
				}
				if record.Round != 1 {
					t.Fatalf("review round = %d, want 1", record.Round)
				}
				if _, err := tx.GetReviewRetry(p.ctx, p.runID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("completed run left a pending retry: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestProductionReviewRetryClearsOnStaleCandidate proves the reconstructed row
// is a delay claim bound to a candidate, never authority: a row left over from
// a superseded candidate is dropped, and the gate proceeds against the current
// candidate rather than honoring the stale deadline.
func TestProductionReviewRetryClearsOnStaleCandidate(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{ReviewSource: p.reviewer, failInspectAt: 2}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("transient observation = %#v, %v", result, err)
	}
	// Rebind the persisted retry to a superseded head, as if a candidate change
	// left it behind; its deadline is still in the future.
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		retry, err := tx.GetReviewRetry(p.ctx, p.runID)
		if err != nil {
			return err
		}
		retry.HeadSHA = "superseded-head"
		retry.Reason = "stale-candidate retry"
		return tx.PutReviewRetry(p.ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	// Restart inside the original delay: the reconstructed row is bound to a
	// different candidate, so it is stale. The gate drops it and proceeds
	// against the current candidate rather than waiting out its deadline.
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("stale-candidate retry did not proceed against the current candidate = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetReviewRetry(p.ctx, p.runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("stale review retry survived: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewRetainsWorkspaceForPendingPreparation(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{
		ReviewSource: p.reviewer, failRequestAfterStart: true,
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("transient preparation = %#v, %v", result, err)
	}
	if faults.requestedWorkspace == "" {
		t.Fatal("review request did not retain a workspace")
	}
	if info, err := os.Stat(faults.requestedWorkspace); err != nil || !info.IsDir() {
		t.Fatalf("retained review workspace = %#v, %v", info, err)
	}
	content, err := os.ReadFile(filepath.Join(faults.requestedWorkspace, "README.md"))
	if err != nil {
		t.Fatalf("read retained candidate content: %v", err)
	}
	if string(content) != "production change\n" {
		t.Fatalf("retained candidate content = %q, want %q", content, "production change\n")
	}
	if status := runGit(t, faults.requestedWorkspace, "status", "--porcelain"); status != "" {
		t.Fatalf("retained candidate worktree is dirty:\n%s", status)
	}
	requestCalls, inspectCalls := faults.requestCalls, faults.inspectCalls
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("preparation retry before backoff = %#v, %v", result, err)
	}
	if faults.requestCalls != requestCalls || faults.inspectCalls != inspectCalls {
		t.Fatalf("preparation retried before backoff: request=%d/%d inspect=%d/%d",
			faults.requestCalls, requestCalls, faults.inspectCalls, inspectCalls)
	}
	p.now = p.now.Add(time.Second)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("preparation retry = %#v, %v", result, err)
	}
	if _, err := os.Stat(faults.requestedWorkspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal review workspace still exists: %v", err)
	}
}

func TestProductionReviewReseedsExistingWorkspaceForUnknownInvocation(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{
		ReviewSource: p.reviewer, failRequestAfterStart: true,
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	target := filepath.Join(
		p.workDir, "production-publication", "review-workspaces",
		string(engine.ProductionReviewInvocationID(p.runID, 1)),
	)
	writeFile(t, target, "pre-upgrade.txt", "unmaterialized\n")

	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("same-invocation recovery = %#v, %v", result, err)
	}
	if faults.requestedWorkspace != target {
		t.Fatalf("requested workspace = %q, want %q", faults.requestedWorkspace, target)
	}
	if _, err := os.Lstat(filepath.Join(target, "pre-upgrade.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-upgrade workspace content survived reseed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(target, "README.md")) //nolint:gosec // test-owned retained-workspace path
	if err != nil || string(content) != "production change\n" {
		t.Fatalf("reseeded candidate content = %q, %v", content, err)
	}
	if status := runGit(t, target, "status", "--porcelain"); status != "" {
		t.Fatalf("reseeded candidate worktree is dirty:\n%s", status)
	}
}

func TestProductionReviewWorkspaceMaterializationFailureIsAtomic(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = faults
	injected := errors.New("injected review workspace materialization failure")
	p.transport.failMaterialization(injected)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)

	if _, err := p.reconcileLanes(); !errors.Is(err, injected) {
		t.Fatalf("materialization failure = %v, want injected error", err)
	}
	if faults.requestCalls != 0 {
		t.Fatalf("review launched after materialization failure: %d requests", faults.requestCalls)
	}
	target := filepath.Join(
		p.workDir, "production-publication", "review-workspaces",
		string(engine.ProductionReviewInvocationID(p.runID, 1)),
	)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed materialization left a durable workspace: %v", err)
	}
}

func TestProductionReviewWorkspaceMaterializationRefusalIsDurable(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = faults
	refused := fmt.Errorf("injected candidate-tree refusal: %w", publish.ErrMaterializationRefused)
	p.transport.failMaterialization(refused)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)

	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	// A configuration-class refusal parks the run (issue #611); it no longer
	// terminalizes the publication task behind a dispute.
	if result.BlockedItemsCreated != 0 || result.PublicationTasksCompleted != 0 {
		t.Fatalf("materialization refusal result = %#v", result)
	}
	if faults.requestCalls != 0 {
		t.Fatalf("review launched after materialization refusal: %d requests", faults.requestCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureConfiguration || failure.Round != 1 {
			t.Fatalf("materialization refusal = %#v", failure)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(
		p.workDir, "production-publication", "review-workspaces",
		string(engine.ProductionReviewInvocationID(p.runID, 1)),
	)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused materialization left a durable workspace: %v", err)
	}
	result, err = p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 0 || result.PublicationTasksCompleted != 0 {
		t.Fatalf("materialization refusal replay = %#v, %v", result, err)
	}
	if faults.requestCalls != 0 {
		t.Fatalf("review launched while replaying materialization refusal: %d requests", faults.requestCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewConfiguration ||
			!item.Offers(domain.ActionAdoptReviewConfiguration) || item.Status != domain.StatusOpen {
			t.Fatalf("materialization refusal item = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewWorkspaceCleanupRefusesSymlinkReplacement(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{
		ReviewSource: p.reviewer, failRequestAfterStart: true,
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(faults.requestedWorkspace); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	marker := filepath.Join(foreign, "must-survive")
	writeFile(t, foreign, "must-survive", "foreign\n")
	if err := os.Symlink(foreign, faults.requestedWorkspace); err != nil {
		t.Fatal(err)
	}
	p.now = p.now.Add(time.Second)
	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("symlink replacement cleanup = %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || //nolint:gosec // test-owned path under t.TempDir
		string(body) != "foreign\n" {
		t.Fatalf("foreign workspace target changed: %q, %v", body, err)
	}
}

func TestProductionReviewContradictionParksWithOneRecoveryItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
		},
	})
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("contradictory review result = %#v, %v", result, err)
	}
	var firstRevision int64
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureContradiction {
			t.Fatalf("contradiction record = %#v", failure)
		}
		digest, err := tx.ReviewFailureBodyDigest(p.ctx, failure.InvocationID)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewContradiction ||
			!item.Offers(domain.ActionRecoverReview) || item.Status != domain.StatusOpen ||
			item.ReviewRecoveryBinding == nil ||
			!item.ReviewRecoveryBinding.Matches(failure, digest) {
			t.Fatalf("contradiction recovery item = %#v", item)
		}
		state, err := tx.ServerState(p.ctx)
		if err != nil {
			return err
		}
		firstRevision = state.Revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("parked contradiction replay = %#v, %v", result, err)
	}
	state, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != firstRevision {
		t.Fatalf("second parked tick moved revision %d -> %d", firstRevision, state.Revision)
	}
}

func TestProductionReviewContradictionStaysParkedAfterDeliveryTiming(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("park contradiction: %v", err)
	}

	itemID := productionReviewItemIDForTest(p.runID, 1)
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(p.ctx, itemID)
		if err != nil {
			return err
		}
		delivery := domain.AttentionDelivery{
			ItemID: itemID, DeviceID: "device-review-recovery", Channel: "ntfy", Attempt: 1,
			SubmittedAt: p.now, Status: domain.DeliverySubmitted,
		}
		if err := tx.PutAttentionDelivery(p.ctx, delivery); err != nil {
			return err
		}
		item, err = item.WithTiming([]domain.AttentionDelivery{delivery})
		if err != nil {
			return err
		}
		item.ItemVersion++
		return tx.PutAttentionItem(p.ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("delivery timing displaced parked contradiction = %#v, %v", result, err)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen || item.Item.ItemVersion != 2 ||
		item.Item.Timing.DeliveryCount != 1 {
		t.Fatalf("timed parked contradiction = %#v", item.Item)
	}
}

// TestProductionLaunchContradictionRaisesRecoveryItem covers a launch-time
// contradiction: persist its class, create no readiness, and keep the task
// live behind one recovery item rather than terminalizing it as a dispute.
func TestProductionLaunchContradictionRaisesRecoveryItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	faults := &faultReviewSource{
		ReviewSource: p.reviewer,
		failRequestWith: &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.New("workspace preparation conformance contradiction"),
		},
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("launch contradiction: %v", err)
	}
	assertPersistedContradictionWithRecovery := func() {
		t.Helper()
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
			if err != nil {
				return err
			}
			if failure.Class != domain.ReviewFailureContradiction {
				t.Fatalf("launch failure class = %#v, want contradiction", failure)
			}
			if _, err := tx.GetAttentionItem(p.ctx,
				domain.ItemID("production-ready-"+string(p.runID))); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("readiness created for a launch contradiction: %w", err)
			}
			item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
			if err != nil {
				return err
			}
			if item.Type != domain.AttentionReviewContradiction ||
				!item.Offers(domain.ActionRecoverReview) {
				t.Fatalf("launch contradiction item = %#v", item)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertPersistedContradictionWithRecovery()
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("resumed parked launch contradiction: %v", err)
	}
	assertPersistedContradictionWithRecovery()
}

func productionReviewItemIDForTest(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("production-review-%s-%d", runID, round))
}

func TestProductionReviewRecoveryAdvancesAndPreservesFailure(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	firstID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(firstID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("park contradiction: %v", err)
	}

	var original domain.ReviewFailure
	var originalDigest domain.Digest
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		original, err = tx.GetReviewFailure(p.ctx, firstID)
		if err != nil {
			return err
		}
		originalDigest, err = tx.ReviewFailureBodyDigest(p.ctx, firstID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.attention.GetAttentionItem(
		p.ctx, productionReviewItemIDForTest(p.runID, 1))
	if err != nil {
		t.Fatal(err)
	}
	deviceID := domain.DeviceID("device-review-recovery")
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Review recovery device",
			Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "recover-review-round-1", DeviceID: deviceID,
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: snapshot.Item.ID, ItemVersion: snapshot.Item.ItemVersion,
			PRHeadSHA:       snapshot.Item.PRHeadSHA,
			ArtifactDigests: snapshot.Item.ArtifactDigests,
			Action:          domain.ActionRecoverReview,
		},
	}); err != nil {
		t.Fatalf("submit recovery: %v", err)
	}

	p.now = p.now.Add(time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("recovered clean review")),
		},
	})
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("recovered review = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.GetReviewFailure(p.ctx, firstID)
		if err != nil {
			return err
		}
		digest, err := tx.ReviewFailureBodyDigest(p.ctx, firstID)
		if err != nil {
			return err
		}
		if failure != original || digest != originalDigest {
			t.Fatalf("original failure changed: %#v/%s, want %#v/%s",
				failure, digest, original, originalDigest)
		}
		record, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if record.Round != 2 || record.InvocationID != secondID {
			t.Fatalf("recovered review record = %#v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewRecoveryAtHardLimitEscalatesUnderDistinctItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	firstID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(firstID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("park contradiction: %v", err)
	}

	contradictionID := productionReviewItemIDForTest(p.runID, 1)
	snapshot, err := p.attention.GetAttentionItem(p.ctx, contradictionID)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := domain.DeviceID("device-hard-limit-recovery")
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Hard-limit recovery device",
			Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "recover-review-at-hard-limit", DeviceID: deviceID,
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: snapshot.Item.ID, ItemVersion: snapshot.Item.ItemVersion,
			PRHeadSHA: snapshot.Item.PRHeadSHA, ArtifactDigests: snapshot.Item.ArtifactDigests,
			Action: domain.ActionRecoverReview,
		},
	}); err != nil {
		t.Fatalf("submit recovery: %v", err)
	}

	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("hard-limit recovery = %#v, %v", result, err)
	}
	exhaustionID := domain.ItemID("production-recovered-review-exhaustion-" + string(p.runID) + "-1")
	if exhaustionID == contradictionID {
		t.Fatal("hard-limit escalation reused the contradiction item id")
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		contradiction, err := tx.GetAttentionItem(p.ctx, contradictionID)
		if err != nil {
			return err
		}
		if contradiction.Status != domain.StatusResolved ||
			contradiction.Type != domain.AttentionReviewContradiction {
			t.Fatalf("resolved contradiction carrier = %#v", contradiction)
		}
		exhaustion, err := tx.GetAttentionItem(p.ctx, exhaustionID)
		if err != nil {
			return err
		}
		if exhaustion.Status != domain.StatusOpen ||
			exhaustion.Type != domain.AttentionReviewDiminishing {
			t.Fatalf("hard-limit escalation = %#v", exhaustion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionNonContradictionHardLimitConvergesOnLegacyItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	firstID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(firstID, fake.ReviewScript{Outcome: fake.OutcomeFail})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("record transient failure: %v", err)
	}

	// Simulate an older daemon that wrote the hard-limit item under the
	// historical round identity, then crashed before terminalizing the task.
	runID := p.runID
	legacyID := productionReviewItemIDForTest(p.runID, 1)
	legacy, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: legacyID, ProjectID: p.projectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason:            "Review exhausted the resolved hard limit of 1 rounds.",
		RequestedDecision: []domain.Action{domain.ActionFinishNow},
		PRHeadSHA:         p.replay.HeadSHA, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.attention.PutItem(p.ctx, legacy); err != nil {
		t.Fatal(err)
	}

	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("legacy hard-limit convergence = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, legacyID)
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewDiminishing || item.Status != domain.StatusOpen {
			t.Fatalf("legacy hard-limit item = %#v", item)
		}
		alternateID := domain.ItemID(
			"production-recovered-review-exhaustion-" + string(p.runID) + "-1")
		if _, err := tx.GetAttentionItem(p.ctx, alternateID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("non-contradiction exhaustion created alternate item: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewRewrittenRequestFailsClosedBeforeRelaunch(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(id, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("never delivered")),
		},
	})
	// Model a persisted request row rewritten while the invocation was
	// interrupted: the recomputed authority digest no longer matches. The
	// engine must fail closed before Inspect can act on the decoded request
	// and relaunch anything from it, so with the gate armed from the first
	// pass the review source must never be inspected at all. Were the gate
	// ordered after Inspect, the scripted clean review would complete instead
	// of parking behind the exact contradiction recovery carrier.
	faults := &faultReviewSource{
		ReviewSource: p.reviewer,
		failAuthorityWith: &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   domain.ErrParentKeyMismatch,
		},
	}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 ||
		result.ReadyItemsCreated != 0 {
		t.Fatalf("rewritten review request = %#v, %v", result, err)
	}
	if faults.inspectCalls != 0 {
		t.Fatalf("rewritten request still drove Inspect %d time(s)", faults.inspectCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Class != domain.ReviewFailureContradiction {
			t.Fatalf("rewritten request failure = %#v", failure)
		}
		digest, err := tx.ReviewFailureBodyDigest(p.ctx, failure.InvocationID)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewContradiction ||
			item.ReviewRecoveryBinding == nil ||
			!item.ReviewRecoveryBinding.Matches(failure, digest) {
			t.Fatalf("rewritten request recovery item = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionVerificationRejectsRecipeNotBoundToProjectImage(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	mismatched := []byte(`{"commands":[["different-check"]],"capture":"none"}`)
	p.room.recipe = mismatched
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{
			p.recipeD:                       true,
			verify.RecipeDigest(mismatched): true,
		},
	)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("mismatched project-image recipe error = %v", err)
	}
	if p.room.reads != 1 || p.room.runs != 0 {
		t.Fatalf("mismatched recipe reads/runs = %d/%d, want 1/0", p.room.reads, p.room.runs)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("mismatched recipe caused effects: %d refs, %d PRs", refs, prs)
	}
}

func TestProductionRecipeExtractionIsBounded(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.recipeReadTimeout = 25 * time.Millisecond
	p.room.read = func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("recipe extraction timeout = %#v, %v", result, err)
	}
	if p.room.reads != 1 || p.room.runs != 0 {
		t.Fatalf("timed-out recipe reads/runs = %d/%d, want 1/0", p.room.reads, p.room.runs)
	}
	if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("immediate timed-out recipe replay = %#v, %v", replay, err)
	}
	if p.room.reads != 1 {
		t.Fatalf("timed-out recipe was read %d times before backoff elapsed, want 1", p.room.reads)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("timed-out recipe extraction caused effects: %d refs, %d PRs", refs, prs)
	}
	p.room.read = nil
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("recipe extraction timeout recovery = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationRestartsAcrossDurableBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seams func(error) productionCrashSeams
	}{
		{"verification", func(err error) productionCrashSeams {
			return productionCrashSeams{afterVerification: func() error { return err }}
		}},
		{"publication", func(err error) productionCrashSeams {
			return productionCrashSeams{afterPublication: func() error { return err }}
		}},
		{"ready", func(err error) productionCrashSeams {
			return productionCrashSeams{afterReady: func() error { return err }}
		}},
		{"terminal", func(err error) productionCrashSeams {
			return productionCrashSeams{afterTerminal: func() error { return err }}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, tc.seams(errors.New("injected crash")), true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("crash seam did not interrupt reconciliation")
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			p.assertReady(t)
			refs, prs := p.forge.counts()
			if refs != 1 || prs != 1 {
				t.Fatalf("restart duplicated effects: %d refs, %d PRs", refs, prs)
			}
			if p.room.reads != 1 || p.room.runs != 1 {
				t.Fatalf("restart repeated recipe extraction/verification: %d/%d", p.room.reads, p.room.runs)
			}
		})
	}
}

func TestAttendedRestartHoldsQueuedUnattendedPublication(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	recoveryCalls := 0
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{reviewRecovery: func(context.Context) error {
			recoveryCalls++
			if recoveryCalls == 1 {
				return errors.New("runtime cleanup temporarily unavailable")
			}
			return nil
		}}, true, nil, domain.ModeAttendedDev, true,
	)
	before, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("attended publication ignored failed review recovery")
	}
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("attended publication hold = %#v, %v", result, err)
	}
	if recoveryCalls != 2 {
		t.Fatalf("attended review recovery calls = %d, want retry", recoveryCalls)
	}
	after, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("attended publication hold moved server state from %#v to %#v", before, after)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("attended publication hold caused effects: %d refs, %d PRs", refs, prs)
	}
	if p.room.reads != 0 || p.room.runs != 0 {
		t.Fatalf("attended publication hold verified work: %d reads, %d runs", p.room.reads, p.room.runs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		if err != nil {
			return err
		}
		if task.Dispatched() {
			return fmt.Errorf("attended publication task status = %q, want pending", task.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The intentional pause is operator-visible run state (issue #394): the
	// hold-only pass records the typed attended-mode hold for the queued
	// task, without a revision bump (checked above) or any forge effect.
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		hold, found, err := tx.GetRunHold(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if !found || hold.Reason != domain.HoldAttendedModeActive {
			t.Errorf("attended publication hold observation = %+v, %v; want %s",
				hold, found, domain.HoldAttendedModeActive)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("resume held publication after unattended restart: %v", err)
	}
	p.assertReady(t)
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("resumed publication effects = %d refs, %d PRs, want 1/1", refs, prs)
	}
	// Resuming and converging clears the hold through forward progress.
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		hold, found, err := tx.GetRunHold(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if found {
			t.Errorf("resumed publication left the hold standing: %+v", hold)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionPublicationRecoversWithoutPrivateDriverReplay(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	var executionExport domain.ExecutionExport
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		executionExport, err = tx.GetExecutionExportRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(
		p.ctx, p.store, executionExport, p.replay,
	); err != nil {
		t.Fatalf("identical atomic completion did not converge: %v", err)
	}
	var task store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if task.Kind != engine.KindProductionPublicationRequested {
		t.Fatalf("atomic completion task kind = %q", task.Kind)
	}
	checkpoint := filepath.Join(t.TempDir(), "freeside-checkpoint.db")
	if err := p.store.Checkpoint(p.ctx, checkpoint); err != nil {
		t.Fatalf("checkpoint atomic completion: %v", err)
	}
	if _, err := p.store.Restore(p.ctx, checkpoint); err != nil {
		t.Fatalf("restore atomic completion: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(p.workDir, "production-driver")); err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewStageDriverAt(filepath.Join(p.workDir, "production-driver"))
	if err != nil {
		t.Fatal(err)
	}
	p.driver = driver
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("SQLite-and-artifact recovery required private replay: %v", err)
	}
	p.assertReady(t)
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
}

func TestUnattendedExecutionExportUsesAtomicPathAfterAttendedRestart(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	result, err := p.reconcileLanes()
	if err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("start result = %#v, %v", result, err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			p.ctx, "production-publication/"+string(p.runID), "conflicting-task", []byte("different"),
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, p.invocation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: p.invocation, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
		ManifestDigest: p.replay.ManifestDigest, RecordedAt: fakePublicationTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(p.ctx, p.dbPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{p.recipeD: true},
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	p.store = restarted
	if err := engine.RecordExecutionExport(p.ctx, p.store, record, p.replay); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("attended-restart completion error = %v, want atomic task conflict", err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionExportRecord(p.ctx, p.invocation)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back execution export lookup = %v, want not found", err)
	}
}

func TestProductionPublicationRestartsAcrossExternalEffectBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		inject func(*productionPublicationHarness)
	}{
		{"transport push", func(p *productionPublicationHarness) { p.transport.failNextPush() }},
		{"pull request create", func(p *productionPublicationHarness) { p.forge.failAfterNextPRCreate() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			tc.inject(p)
			if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
				t.Fatalf("contained external-effect failure = %#v, %v", result, err)
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			p.assertReady(t)
			if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
				t.Fatalf("restart duplicated effects: %d refs, %d PRs", refs, prs)
			}
		})
	}
}

func TestFinalizedProductionPublicationSurvivesLaterTrustDrift(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error { return errors.New("stop after durable publication outcome") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("publication outcome seam did not interrupt reconciliation")
	}
	reviseWaivedTrustProfile(t, p.store)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("finalize durable publication after trust drift: %v", err)
	}
	p.assertReady(t)
}

func TestReadyProductionPublicationWinsOverLaterExternalConflict(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterReady: func() error { return errors.New("stop after durable ready item") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready-item seam did not interrupt reconciliation")
	}
	p.forge.clearRefs()
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("recover ready publication after external conflict = %#v, %v", result, err)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ready recovery created contradictory blocked item: %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
		t.Fatalf("ready recovery changed external effects: %d refs/%d PRs", refs, prs)
	}
	if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("ready-conflict replay = %#v, %v", replay, err)
	}
}

func TestReadyProductionPublicationMissingPrerequisiteFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		arg   any
	}{
		{"outcome", "DELETE FROM inbox WHERE kind = ?", publish.IntentKindOutcome},
		{"checkpoint", "DELETE FROM inbox WHERE idempotency_key = ?", "production-verification/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.workflow = p.newEngine(t, productionCrashSeams{
				afterReady: func() error { return errors.New("stop after durable ready item") },
			}, true)
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("ready-item seam did not interrupt reconciliation")
			}
			arg := tc.arg
			if tc.name == "checkpoint" {
				arg = tc.arg.(string) + string(p.runID)
			}
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			result, err := raw.ExecContext(p.ctx, tc.query, arg)
			closeErr := raw.Close()
			if err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			if removed, err := result.RowsAffected(); err != nil || removed != 1 {
				t.Fatalf("removed prerequisite rows = %d, %v, want 1", removed, err)
			}
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("orphaned ready recovery error = %v, want parent-key mismatch", err)
			}
			if _, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("orphaned ready recovery created blocked item: %v", err)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
				t.Fatalf("orphaned ready recovery changed external effects: %d refs/%d PRs", refs, prs)
			}
		})
	}
}

func TestFinalizedProductionPublicationSurvivesLaterRecipeRevocation(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterReady: func() error { return errors.New("stop after durable ready item") },
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("ready seam did not interrupt reconciliation")
	}
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
	)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("finalize durable ready item after recipe revocation = %#v, %v", result, err)
	}
	p.assertReady(t)
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finalized ready item created contradictory blocked item: %v", err)
	}
}

// TestPostPublicationRecipeRevocationHoldsWhenStoreAlsoRevoked proves the
// readiness reconstruction boundary re-gates the recipe through the STORE, not
// only the engine: when a crash leaves a published run with no ready item and
// the recipe is revoked at both the engine and a freshly reopened store (the
// realistic restart shape), recovery takes the durable recipe-revoked hold
// rather than failing the reconcile lane. The sibling
// ...RegatesRecipeAuthorityBeforeReadiness revokes only the engine, so its
// store still approves the clean-verification proof and never exercises the
// store's recipe gate on this path. With the store revoked, that gate returns
// ErrUnapprovedRecipe from RecordCheckProof; the recovery path must route it to
// the hold, so it defers the recipe-gated proof persistence until after the
// recipe-approval decision instead of writing the proof up front (issue #527,
// decision 2). Without that deferral the error escapes as a lane-fatal reconcile
// failure and the run never reaches its hold.
func TestPostPublicationRecipeRevocationHoldsWhenStoreAlsoRevoked(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after publication, before readiness")
		},
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("afterPublication seam did not interrupt reconciliation")
	}
	revoked := map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true}
	p.reopenStoreWithApprovedRecipes(t, revoked)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revoked)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 1 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("post-publication recipe revocation = %#v, %v", result, err)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked recipe created readiness: %v", err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil || !strings.Contains(hold.Item.Reason, "no longer approves the verification recipe") {
		t.Fatalf("recipe revocation hold = %#v, %v", hold, err)
	}
}

func TestProductionReviewRegatesRecipeAuthorityBeforeReadiness(t *testing.T) {
	t.Parallel()
	// A candidate reviews clean and publishes, but a crash between publication and
	// readiness leaves no ready item. If recipe approval is revoked before the
	// recovery pass, readiness is re-gated on the recipe and held, never derived
	// (issue #527: the readiness path re-gates recipe approval after the clean
	// review pass); restoring approval recovers it to ready.
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after publication, before readiness")
		},
	}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("afterPublication seam did not interrupt reconciliation")
	}
	revokedRecipes := map[domain.Digest]bool{
		productionDigest([]byte("unrelated recipe")): true,
	}
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 1 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("readiness after recipe revocation = %#v, %v", result, err)
	}
	if _, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked review created readiness: %v", err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil || !strings.Contains(hold.Item.Reason, "no longer approves") {
		t.Fatalf("revoked review hold = %#v, %v", hold, err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err = p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("review after recipe reapproval = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationConflictIsDurablyHeld(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.conflictNextPush()
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 1 {
		t.Fatalf("conflicted publication hold = %#v, %v", result, err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hold.Item.Reason, "external branch or pull request conflicts") ||
		!slices.Equal(hold.Item.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		t.Fatalf("publication conflict hold = %#v", hold.Item)
	}
	fetchesBeforeReplay := p.transport.fetchCount()
	if replay, err := p.reconcileLanes(); err != nil ||
		replay.PublicationTasksCompleted != 0 || replay.BlockedItemsCreated != 0 {
		t.Fatalf("idempotent publication conflict hold = %#v, %v", replay, err)
	}
	if fetches := p.transport.fetchCount(); fetches != fetchesBeforeReplay {
		t.Fatalf("held publication immediate replay fetched %d bases, want unchanged %d",
			fetches, fetchesBeforeReplay)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 0 {
		t.Fatalf("conflicted publication effects = %d refs/%d PRs", refs, prs)
	}
	p.forge.clearRefs()
	p.now = p.now.Add(time.Minute)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("recover publication after conflict repair: %v", err)
	}
	p.assertReady(t)
	hold, err = p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Item.Status != domain.StatusSuperseded {
		t.Fatalf("recovered conflict hold status = %q, want superseded", hold.Item.Status)
	}
}

func TestProductionPublicationTransientFailureBacksOffWithoutStoppingEngine(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failFetch(&net.DNSError{Err: "temporary", Name: "github.com"})

	result, err := p.reconcileLanes()
	if err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("transient publication reconcile = %#v, %v", result, err)
	}
	if fetches := p.transport.fetchCount(); fetches != 1 {
		t.Fatalf("transient publication fetches = %d, want 1", fetches)
	}
	if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
		t.Fatalf("immediate transient replay = %#v, %v", replay, err)
	}
	if fetches := p.transport.fetchCount(); fetches != 1 {
		t.Fatalf("transient backoff fetched %d bases, want 1", fetches)
	}

	p.transport.failFetch(nil)
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("transient publication recovery = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationMutableAppAuthorityBacksOff(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.tokens = inactiveIntegrationTokenSource{}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)

	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("inactive App authority reconcile = %#v, %v", result, err)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 0 {
		t.Fatalf("inactive App authority effects = %d refs/%d PRs, want converged push only", refs, prs)
	}

	p.tokens = integrationTokenSource{}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("restored App authority reconcile = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationPermanentFetchRefusalIsHeld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "missing base", err: publish.ErrRemoteMissingBase},
		{name: "trust profile drift", err: publish.ErrTrustProfileDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			p.transport.failFetch(fmt.Errorf("fetch refused: %w", tc.err))

			if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("permanent fetch refusal reconcile = %#v, %v", result, err)
			}
			p.transport.failFetch(nil)
			p.now = p.now.Add(time.Minute)
			if result, err := p.reconcileLanes(); err != nil ||
				result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
				t.Fatalf("repaired permanent fetch refusal = %#v, %v", result, err)
			}
			p.assertReady(t)
		})
	}
}

func TestProductionPublicationHoldAdvancesToDefinitiveBlock(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failFetch(fmt.Errorf("base disappeared: %w", publish.ErrRemoteMissingBase))
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("initial repairable hold = %#v, %v", result, err)
	}
	itemID := domain.ItemID("production-publish-blocked-" + string(p.runID))
	hold, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}

	p.transport.failFetch(nil)
	p.room.fail = true
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 1 || result.BlockedItemsCreated != 1 {
		t.Fatalf("definitive block after repaired hold = %#v, %v", result, err)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.ItemVersion != hold.Item.ItemVersion+1 ||
		blocked.Item.Reason != "Verification or current policy findings blocked production publication." ||
		!slices.Equal(blocked.Item.RequestedDecision, []domain.Action{
			domain.ActionInspectTrustFailure, domain.ActionStop,
		}) || blocked.Item.Status != domain.StatusOpen {
		t.Fatalf("definitive successor = %#v", blocked.Item)
	}
	if blocked.Item.Timing != hold.Item.Timing ||
		!reflect.DeepEqual(blocked.Item.ConversationID, hold.Item.ConversationID) ||
		!reflect.DeepEqual(blocked.Item.ExpiresWhen, hold.Item.ExpiresWhen) {
		t.Fatalf("definitive successor lost hold metadata: before=%#v after=%#v",
			hold.Item, blocked.Item)
	}
}

func TestProductionPublicationReleaseFailurePreservesOutcome(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterLockRelease: func() error { return errors.New("injected lock release failure") },
	}, true)
	p.startAndRecordExport(t)

	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 ||
		result.ReadyItemsCreated != 1 || result.LastPRNumber <= 0 {
		t.Fatalf("release failure result = %#v, %v", result, err)
	}
	p.assertReady(t)
}

func TestProductionPublicationCorruptCheckpointStillFailsLoud(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, inserted, err := tx.RecordInbox(
			p.ctx,
			"production-verification/"+string(p.runID),
			"production_verification_checkpoint",
			[]byte(`{"version":`),
		)
		if err == nil && !inserted {
			return errors.New("corrupt checkpoint unexpectedly existed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("corrupt production checkpoint error = %v, want parent-key mismatch", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("corrupt production checkpoint caused effects: %d refs/%d PRs", refs, prs)
	}
}

func TestProductionPublicationReadyPrecedesHoldSupersession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		action domain.Action
	}{
		{name: "open"},
		{name: "dismissed", action: domain.ActionDismiss},
		{name: "stopped", action: domain.ActionStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			p.transport.conflictNextPush()
			if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("conflicted publication hold = %#v, %v", result, err)
			}
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{
				afterReady: func() error { return errors.New("stop after durable ready item") },
			}, true)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("ready-item seam did not interrupt held-publication recovery")
			}
			hold, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if hold.Item.Status != domain.StatusOpen {
				t.Fatalf("hold status before ready recovery = %q, want open", hold.Item.Status)
			}
			ready, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-ready-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if tc.action != "" {
				deviceID := domain.DeviceID("device-ready-" + tc.name)
				if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
					return tx.PutDevice(p.ctx, domain.Device{
						ID: deviceID, DisplayName: "Ready recovery device",
						Status: domain.DeviceActive, PairedAt: fakePublicationTime,
					})
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
					CommandID: "ready-" + tc.name, DeviceID: deviceID,
					ExpectedEntityVersion: ready.EntityVersion,
					Payload: signet.DecisionPayload{
						ItemID: ready.Item.ID, Action: tc.action,
						ItemVersion: ready.Item.ItemVersion, PRHeadSHA: ready.Item.PRHeadSHA,
						ArtifactDigests: ready.Item.ArtifactDigests,
					},
				}); err != nil {
					t.Fatal(err)
				}
				ready, err = p.attention.GetAttentionItem(p.ctx, ready.Item.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ready.Item.Status == domain.StatusOpen || ready.Item.DecidedAt == nil {
					t.Fatalf("ready successor = %#v", ready.Item)
				}
			}
			successor := ready.Item
			p.forge.clearRefs()
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			result, err := p.reconcileLanes()
			if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
				t.Fatalf("recover ready publication after later conflict = %#v, %v", result, err)
			}
			hold, err = p.attention.GetAttentionItem(p.ctx, hold.Item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hold.Item.Status != domain.StatusSuperseded {
				t.Fatalf("recovered hold status = %q, want superseded", hold.Item.Status)
			}
			recovered, err := p.attention.GetAttentionItem(p.ctx, ready.Item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recovered.Item, successor) {
				t.Fatalf("ready successor changed on recovery:\n got: %#v\nwant: %#v",
					recovered.Item, successor)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 1 {
				t.Fatalf("ready recovery changed external effects: %d refs/%d PRs", refs, prs)
			}
		})
	}
}

func TestRecipeRevocationRetainsPendingPublicationIntent(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failNextPush()
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("contained transport failure = %#v, %v", result, err)
	}
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
	)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 1 {
		t.Fatalf("revoked recipe with pending intent = %#v, %v", result, err)
	}
	hold, err := p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hold.Item.Reason, "durably held") ||
		len(hold.Item.EvidenceSnapshot) != 0 ||
		!slices.Equal(hold.Item.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		t.Fatalf("pending intent hold = %#v", hold.Item)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		if err != nil {
			return err
		}
		if task.Dispatched() {
			t.Errorf("pending-intent task status = %q, want pending", task.Status)
		}
		_, err = tx.GetInbox(p.ctx, string(p.invocation))
		if err == nil {
			return errors.New("pending-intent terminal unexpectedly exists")
		}
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("read pending-intent terminal: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("idempotent revoked-recipe hold = %#v, %v", result, err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("recover retained publication after recipe approval: %v", err)
	}
	p.assertReady(t)
	hold, err = p.attention.GetAttentionItem(
		p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Item.Status != domain.StatusSuperseded {
		t.Fatalf("recovered publication hold status = %q, want superseded", hold.Item.Status)
	}
}

func TestProductionPublicationHoldRefreshesWhenCauseChanges(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.conflictNextPush()
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("external-conflict hold = %#v, %v", result, err)
	}
	itemID := domain.ItemID("production-publish-blocked-" + string(p.runID))
	conflict, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	p.forge.clearRefs()
	p.workflow = p.newEngineWithApprovedRecipes(
		t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
	)
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("recipe-revocation hold refresh = %#v, %v", result, err)
	}
	revoked, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Item.ItemVersion != conflict.Item.ItemVersion+1 ||
		!strings.Contains(revoked.Item.Reason, "current trust no longer approves") ||
		strings.Contains(revoked.Item.Reason, "external branch or pull request conflicts") {
		t.Fatalf("refreshed publication hold = %#v", revoked.Item)
	}
	beforeReplay, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("idempotent refreshed hold = %#v, %v", result, err)
	}
	afterReplay, err := p.store.ServerState(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay != beforeReplay {
		t.Fatalf("refreshed hold replay moved server state from %#v to %#v", beforeReplay, afterReplay)
	}
}

func TestProductionExportSurvivesUntilPublicationComposerReturns(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.workflow = p.newEngine(t, productionCrashSeams{}, false)
	p.startAndRecordExport(t)
	// The reconcile pass itself must refuse: acceptance of a production
	// terminal is what needs the composed publication lane, so the refusal
	// must not depend on the publication pass this engine cannot run.
	if _, err := p.workflow.Reconcile(p.ctx); err == nil ||
		!strings.Contains(err.Error(), "publication workflow is not configured") {
		t.Fatalf("pre-publication completion error = %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("uncomposed publication caused effects: %d/%d", refs, prs)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

func TestProductionVerificationAndHeadMismatchNeverReachExternalEffects(t *testing.T) {
	t.Parallel()
	t.Run("blocked reason must match checkpoint authority", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		blocked.Item.Reason = "Current trust state definitively blocked publication."
		forged, err := json.Marshal(blocked.Item)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite", p.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		result, err := raw.ExecContext(
			p.ctx, "UPDATE attention_items SET body = ? WHERE id = ?", string(forged), blocked.Item.ID,
		)
		closeErr := raw.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			t.Fatalf("forged blocked rows = %d, %v", changed, err)
		}
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("impossible blocked reason = %v, want parent-key mismatch", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("impossible blocked reason caused forge effects: %d/%d", refs, prs)
		}
	})

	t.Run("open blocked item survives recipe revocation", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(blocked.Item.EvidenceSnapshot) == 0 {
			t.Fatal("verification block carried no evidence")
		}
		p.workflow = p.newEngineWithApprovedRecipes(
			t, productionCrashSeams{}, true,
			map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
		)
		result, err := p.reconcileLanes()
		if err != nil || result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 {
			t.Fatalf("recover blocked item after recipe revocation = %#v, %v", result, err)
		}
		current, err := p.attention.GetAttentionItem(p.ctx, blocked.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(current.Item, blocked.Item) {
			t.Fatalf("blocked item changed after recipe revocation: %#v", current.Item)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("blocked recovery caused forge effects: %d/%d", refs, prs)
		}
	})

	t.Run("resolved blocked item survives recipe revocation", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.workflow = p.newEngine(t, productionCrashSeams{
			afterBlocked: func() error { return errors.New("stop after blocked item") },
		}, true)
		p.startAndRecordExport(t)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("blocked-item seam did not interrupt reconciliation")
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		deviceID := domain.DeviceID("device-blocked-recovery")
		if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			return tx.PutDevice(p.ctx, domain.Device{
				ID: deviceID, DisplayName: "Blocked recovery device",
				Status: domain.DeviceActive, PairedAt: fakePublicationTime,
			})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
			CommandID: "stop-blocked-recovery", DeviceID: deviceID,
			ExpectedEntityVersion: blocked.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: blocked.Item.ID, Action: domain.ActionStop,
				ItemVersion: blocked.Item.ItemVersion, PRHeadSHA: blocked.Item.PRHeadSHA,
				ArtifactDigests: blocked.Item.ArtifactDigests,
			},
		}); err != nil {
			t.Fatal(err)
		}
		resolvedSnapshot, err := p.attention.GetAttentionItem(p.ctx, blocked.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
		resolved := resolvedSnapshot.Item

		p.workflow = p.newEngineWithApprovedRecipes(
			t, productionCrashSeams{}, true,
			map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
		)
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatalf("recover resolved blocked item: %v", err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("resolved-block recovery result = %#v", result)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			if err != nil {
				return err
			}
			if !task.Dispatched() {
				t.Errorf("recovered blocked task status = %q, want dispatched", task.Status)
			}
			terminal, err := tx.GetInbox(p.ctx, string(p.invocation))
			if err != nil {
				return err
			}
			if terminal.Kind != productionTerminalKind {
				t.Errorf("recovered blocked terminal kind = %q", terminal.Kind)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("resolved-block recovery caused forge effects: %d/%d", refs, prs)
		}
		current, err := p.attention.GetAttentionItem(p.ctx, resolved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(current.Item, resolved) {
			t.Fatalf("resolved blocked item changed on recovery: %#v", current.Item)
		}
		if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
			t.Fatalf("resolved-block replay = %#v, %v", replay, err)
		}
	})

	t.Run("verification failure", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.room.fail = true
		p.startAndRecordExport(t)
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("blocked result = %#v", result)
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		wantActions := []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop}
		if !slices.Equal(blocked.Item.RequestedDecision, wantActions) {
			t.Fatalf("blocked actions = %v, want %v", blocked.Item.RequestedDecision, wantActions)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			task, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			if err != nil {
				return err
			}
			if !task.Dispatched() {
				t.Errorf("blocked publication task status = %q, want dispatched", task.Status)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var terminal store.QueueEntry
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			terminal, err = tx.GetInbox(p.ctx, string(p.invocation))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if terminal.Kind != productionTerminalKind {
			t.Fatalf("blocked terminal kind = %q", terminal.Kind)
		}
		beforeReplay, err := p.store.ServerState(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
			t.Fatalf("blocked replay = %#v, %v", replay, err)
		}
		afterReplay, err := p.store.ServerState(p.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if afterReplay != beforeReplay {
			t.Fatalf("blocked replay moved server state from %#v to %#v", beforeReplay, afterReplay)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("unverified candidate caused effects: %d/%d", refs, prs)
		}
	})
	for _, tc := range []struct {
		name            string
		checkpointFirst bool
	}{{"revoked project-image recipe", false}, {"recipe revoked after checkpoint", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			if tc.checkpointFirst {
				p.workflow = p.newEngine(t, productionCrashSeams{
					afterVerification: func() error { return errors.New("stop after checkpoint") },
				}, true)
				if _, err := p.reconcileLanes(); err == nil {
					t.Fatal("verification seam did not stop after checkpoint")
				}
			}
			verificationRuns := p.room.runs
			p.workflow = p.newEngineWithApprovedRecipes(
				t, productionCrashSeams{}, true,
				map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true},
			)
			result, err := p.reconcileLanes()
			if err != nil {
				t.Fatal(err)
			}
			if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
				result.ReadyItemsCreated != 0 {
				t.Fatalf("revoked-recipe result = %#v", result)
			}
			blocked, err := p.attention.GetAttentionItem(
				p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(blocked.Item.Reason, "no longer approves") {
				t.Fatalf("revoked-recipe reason = %q", blocked.Item.Reason)
			}
			if len(blocked.Item.EvidenceSnapshot) != 0 {
				t.Fatalf("revoked-recipe evidence = %#v, want none", blocked.Item.EvidenceSnapshot)
			}
			if p.room.runs != verificationRuns {
				t.Fatalf("revoked recipe ran %d additional verification commands", p.room.runs-verificationRuns)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatalf("revoked recipe caused effects: %d/%d", refs, prs)
			}
			if replay, err := p.reconcileLanes(); err != nil || replay != (engine.ReconcileResult{}) {
				t.Fatalf("revoked-recipe replay = %#v, %v", replay, err)
			}
		})
	}
	t.Run("target base advanced before publication", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.audit.AuditedCommitSHA = strings.Repeat("9", 40)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		p.startAndRecordExport(t)
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if result.ResultsAccepted != 1 || result.BlockedItemsCreated != 1 ||
			result.ReadyItemsCreated != 0 {
			t.Fatalf("advanced-base result = %#v", result)
		}
		blocked, err := p.attention.GetAttentionItem(
			p.ctx, domain.ItemID("production-publish-blocked-"+string(p.runID)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(blocked.Item.Reason, "target base advanced") {
			t.Fatalf("advanced-base reason = %q", blocked.Item.Reason)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("advanced base caused effects: %d/%d", refs, prs)
		}
		var pending []store.QueueEntry
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			pending, err = tx.ListPendingOutbox(p.ctx, publish.IntentKindPublication)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("advanced base left publication intents: %#v", pending)
		}
	})
	t.Run("head mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, strings.Repeat("f", 40))
		record := p.startExecutionExport(t, strings.Repeat("f", 40))
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "replay disagrees") {
			t.Fatalf("head mismatch error = %v", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("mismatched head caused effects: %d/%d", refs, prs)
		}
	})
	t.Run("replay policy mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.replay.ImportOptions.Policy.Allowlist = nil
		record := p.startExecutionExport(t, p.replay.HeadSHA)
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "import options disagree") {
			t.Fatalf("replay policy mismatch error = %v", err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetExecutionExportRecord(p.ctx, p.invocation)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("policy-mismatched export lookup = %v, want not found", err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("policy-mismatched task lookup = %v, want not found", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("policy-mismatched replay caused effects: %d/%d", refs, prs)
		}
	})
	t.Run("replay commit date mismatch", func(t *testing.T) {
		p := newProductionPublicationHarness(t, "")
		p.replay.ImportOptions.CommitDate = p.replay.ImportOptions.CommitDate.Add(time.Minute)
		record := p.startExecutionExport(t, p.replay.HeadSHA)
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err == nil ||
			!strings.Contains(err.Error(), "replay disagrees") {
			t.Fatalf("replay commit-date mismatch error = %v", err)
		}
		if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
			t.Fatalf("commit-date-mismatched replay caused effects: %d/%d", refs, prs)
		}
	})
}

func TestProductionPublicationBackupBindsReplayBlobs(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	// Interrupt after the task and verification checkpoint exist so the
	// outbox extractor can be checked against the exact replay blob set.
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("stop after checkpoint") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("verification seam did not stop")
	}
	var task store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("publication tasks = %d", len(entries))
		}
		task = entries[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	digests, err := engine.ProductionPublicationBackupPayloadDigests(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) < 2 {
		t.Fatalf("backup digests = %v, want manifest and workspace blobs", digests)
	}
	var payload map[string]any
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	replay, ok := payload["replay"].(map[string]any)
	if !ok {
		t.Fatalf("task replay payload = %#v, want object", payload["replay"])
	}
	replay["manifest_digest"] = "sha256:" + strings.Repeat("0", 64)
	tampered, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	task.Payload = tampered
	if _, err := engine.ProductionPublicationBackupPayloadDigests(task); err == nil {
		t.Fatal("backup extractor accepted replay metadata whose role digest was tampered")
	}
}

// TestDowngradedProductionMarkerQuarantinesOnlyItsRun is the #424 regression:
// a marker written by a newer daemon (here, an unknown future version) must
// not end reconciliation on every pass. The run behind it leaves the
// production lane behind a durable notice, while the healthy run in the same
// store still executes, verifies, and publishes.
func TestDowngradedProductionMarkerQuarantinesOnlyItsRun(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	downgraded := domain.RunID("run-downgraded-marker")
	futureVersion := "freeside.production-invocation/v9"
	seedFutureVersionProductionRun(t, p, downgraded, futureVersion)

	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatalf("reconcile beside an unreadable marker: %v", err)
	}
	if result.ResultsAccepted != 1 || result.ReadyItemsCreated != 1 ||
		result.PublicationTasksCompleted != 1 || result.LastPRNumber == 0 {
		t.Fatalf("healthy run result = %#v", result)
	}
	p.assertReady(t)

	item := productionQuarantineItem(t, p, downgraded)
	if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen ||
		item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(downgraded) {
		t.Fatalf("quarantine item = %#v", item)
	}
	// The marker is the untrusted input: its stored text never reaches the
	// operator-facing reason.
	if strings.Contains(item.Reason, futureVersion) || strings.Contains(item.Reason, string(downgraded)) {
		t.Fatalf("quarantine reason echoes marker payload: %q", item.Reason)
	}

	// The restart case: a later pass converges on the one notice and still
	// reports no error.
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("replayed reconcile beside an unreadable marker: %v", err)
	}
	if replayed := productionQuarantineItem(t, p, downgraded); replayed.ItemVersion != item.ItemVersion {
		t.Fatalf("replayed pass rewrote the notice: %#v", replayed)
	}
}

// seedFutureVersionProductionRun writes the run and ownership marker a newer
// daemon would have committed: the lane's own key and kind, carrying a marker
// version this binary does not know.
func seedFutureVersionProductionRun(
	t *testing.T, p *productionPublicationHarness, runID domain.RunID, version string,
) {
	t.Helper()
	run := domain.Run{
		ID: runID, ProjectID: p.projectID,
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Stages: []domain.Stage{{
			ID: domain.StageID("implement-" + string(runID)), RunID: runID,
			Name: "implement", Attempts: []domain.Attempt{},
		}},
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(p.ctx, run)
	}); err != nil {
		t.Fatalf("seed downgraded run: %v", err)
	}
	payload := fmt.Sprintf(
		`{"version":%q,"invocation_id":"inv-implement-%s","run_id":%q,"stage_id":"implement-%s"}`,
		version, runID, runID, runID,
	)
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			p.ctx, "inv-implement-"+string(runID),
			engine.KindProductionInvocationRequested, []byte(payload),
		)
		return err
	}); err != nil {
		t.Fatalf("seed downgraded marker: %v", err)
	}
}

func productionQuarantineItem(
	t *testing.T, p *productionPublicationHarness, runID domain.RunID,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(
			p.ctx, domain.ItemID("production-marker-quarantined-1-"+string(runID)))
		return err
	}); err != nil {
		t.Fatalf("read quarantine item for %q: %v", runID, err)
	}
	return item
}

// TestQuarantinedMarkerHoldsAndReleasesThePublicationLane covers the half of
// #424 the reconcile loop reaches only after execution: a run whose marker
// stops authenticating must not publish, because the publication lane
// re-gates its own authority and never reads the marker. It also pins the
// release: when the marker reads again, the run publishes and the notice is
// retired rather than left contradicting the run's own outcome.
func TestQuarantinedMarkerHoldsAndReleasesThePublicationLane(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)

	// Hold the publication lane so its task is committed and left pending.
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, nil, domain.ModeUnattended, true,
	)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("accept under a held publication lane: %v", err)
	}

	markerKey := "inv-implement-" + string(p.runID)
	original := readOutboxPayload(t, p, markerKey)
	writeOutboxPayload(t, p, markerKey, []byte(fmt.Sprintf(
		`{"version":"freeside.production-invocation/v9","invocation_id":%q,"run_id":%q,"stage_id":%q}`,
		markerKey, p.runID, "implement-"+string(p.runID),
	)))

	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile with a quarantined marker: %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("quarantined run published: %d refs/%d PRs", refs, prs)
	}
	held := productionQuarantineItem(t, p, p.runID)
	if held.Status != domain.StatusOpen {
		t.Fatalf("quarantine notice = %#v", held)
	}

	// The upgrade: the marker reconstructs again.
	writeOutboxPayload(t, p, markerKey, original)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile after the marker reads again: %v", err)
	}
	p.assertReady(t)
	released := productionQuarantineItem(t, p, p.runID)
	if released.Status != domain.StatusSuperseded || released.ItemVersion <= held.ItemVersion {
		t.Fatalf("quarantine notice after recovery = %#v", released)
	}
}

// TestUnreadablePublicationTaskDoesNotEndTheEngineLoop is the sibling row of
// the marker: a downgrade meets newer publication tasks too, and joining that
// decode failure would end Engine.Run on every pass for as long as the row
// exists.
func TestUnreadablePublicationTaskDoesNotEndTheEngineLoop(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	stranded := domain.RunID("run-unreadable-task")
	seedFutureVersionProductionRun(t, p, stranded, "freeside.production-invocation/v2")
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			p.ctx, "production-publication/"+string(stranded),
			engine.KindProductionPublicationRequested,
			[]byte(`{"version":9,"run_id":"run-unreadable-task"}`),
		)
		return err
	}); err != nil {
		t.Fatalf("seed unreadable task: %v", err)
	}

	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile beside an unreadable publication task: %v", err)
	}
	p.assertReady(t)

	var item domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(
			p.ctx, domain.ItemID("production-task-quarantined-1-"+string(stranded)))
		return err
	}); err != nil {
		t.Fatalf("read task quarantine item: %v", err)
	}
	if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen {
		t.Fatalf("task quarantine item = %#v", item)
	}
}

func readOutboxPayload(t *testing.T, p *productionPublicationHarness, key string) []byte {
	t.Helper()
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	err = raw.QueryRowContext(
		p.ctx, `SELECT payload FROM outbox WHERE idempotency_key = ?`, key).Scan(&payload)
	if closeErr := raw.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	return payload
}

func writeOutboxPayload(t *testing.T, p *productionPublicationHarness, key string, payload []byte) {
	t.Helper()
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := raw.ExecContext(
		p.ctx, `UPDATE outbox SET payload = ? WHERE idempotency_key = ?`, payload, key)
	closeErr := raw.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		t.Fatalf("rewrote %d marker rows, %v, want 1", updated, err)
	}
}

// TestUnreadablePublicationTaskHoldsAndReleases: the task row's hold has the
// same lifecycle as the marker's. Its notice must be retired while the row is
// still pending, because a task that completes leaves the pending scan and no
// later pass would reach it.
func TestUnreadablePublicationTaskHoldsAndReleases(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, nil, domain.ModeUnattended, true,
	)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("accept under a held publication lane: %v", err)
	}

	taskKey := "production-publication/" + string(p.runID)
	original := readOutboxPayload(t, p, taskKey)
	writeOutboxPayload(t, p, taskKey, []byte(`{"version":"freeside.production-publication/v9"}`))

	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile with an unreadable task: %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("unreadable task published: %d refs/%d PRs", refs, prs)
	}
	held := productionItemRecord(t, p, "production-task-quarantined-1-"+string(p.runID))
	if held.Status != domain.StatusOpen {
		t.Fatalf("task notice = %#v", held)
	}

	writeOutboxPayload(t, p, taskKey, original)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile after the task reads again: %v", err)
	}
	p.assertReady(t)
	released := productionItemRecord(t, p, "production-task-quarantined-1-"+string(p.runID))
	if released.Status != domain.StatusSuperseded {
		t.Fatalf("task notice after recovery = %#v", released)
	}
}

// TestRemovedMarkerRetiresTheQuarantineNotice: removing the bad row is the
// other repair an operator can make. The run leaves the lane for good, so the
// notice must not outlive the hold it describes while the already-durable
// publication task finishes.
func TestRemovedMarkerRetiresTheQuarantineNotice(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, nil, domain.ModeUnattended, true,
	)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("accept under a held publication lane: %v", err)
	}

	markerKey := "inv-implement-" + string(p.runID)
	writeOutboxPayload(t, p, markerKey, []byte(`{"run_id":"wrong"}`))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile with a quarantined marker: %v", err)
	}
	held := productionItemRecord(t, p, "production-marker-quarantined-1-"+string(p.runID))
	if held.Status != domain.StatusOpen {
		t.Fatalf("marker notice = %#v", held)
	}

	deleteOutboxRow(t, p, markerKey)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile after the marker row was removed: %v", err)
	}
	released := productionItemRecord(t, p, "production-marker-quarantined-1-"+string(p.runID))
	if released.Status != domain.StatusSuperseded {
		t.Fatalf("marker notice after repair = %#v", released)
	}
}

func productionItemRecord(
	t *testing.T, p *productionPublicationHarness, id string,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(p.ctx, domain.ItemID(id))
		return err
	}); err != nil {
		t.Fatalf("read attention item %q: %v", id, err)
	}
	return item
}

func deleteOutboxRow(t *testing.T, p *productionPublicationHarness, key string) {
	t.Helper()
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := raw.ExecContext(p.ctx, `DELETE FROM outbox WHERE idempotency_key = ?`, key)
	closeErr := raw.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if removed, err := result.RowsAffected(); err != nil || removed != 1 {
		t.Fatalf("removed %d rows, %v, want 1", removed, err)
	}
}

// publicationBoundaryWait bounds the waits in the scheduling tests below. It
// is a liveness cap, not a timing assertion: every wait is on a channel the
// test itself closes, so a healthy run reaches it immediately.
const publicationBoundaryWait = 30 * time.Second

// submitUnrelatedRun queues a second production run against the same store and
// driver, so a test can prove the reconcile loop advances work that has
// nothing to do with the publication task it parked.
func (p *productionPublicationHarness) submitUnrelatedRun(
	t *testing.T, runID domain.RunID,
) domain.InvocationID {
	t.Helper()
	spec, policy, resolved := registerSubmissionArtifactsWithPaths(
		t, p.store, string(runID), "README.md",
	)
	submitted, err := engine.SubmitProductionRun(p.ctx, p.store, engine.ProductionRunSpec{
		RunID: runID, ProjectID: p.projectID, SpecArtifactID: spec.ID,
		PolicyArtifactID: policy.ID, ResolvedPolicy: resolved,
		Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.driver.Script(submitted.InvocationID, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{HeadSHA: p.replay.HeadSHA, Summary: "Claude export completed."},
	})
	return submitted.InvocationID
}

// TestBlockedProductionPublicationLeavesTheReconcileLoopFree is the scheduling
// guarantee behind splitting the lanes (issue #425): a publication parked in
// an external boundary holds only its own loop, so the reconcile loop still
// dispatches an unrelated invocation, and the parked task finishes normally
// once the boundary returns.
func TestBlockedProductionPublicationLeavesTheReconcileLoopFree(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	entered, release := p.transport.blockNextFetch()
	publication := make(chan error, 1)
	go func() {
		_, err := p.workflow.ReconcileProductionPublications(p.ctx)
		publication <- err
	}()
	select {
	case <-entered:
	case err := <-publication:
		t.Fatalf("publication pass returned without reaching the transport: %v", err)
	case <-time.After(publicationBoundaryWait):
		t.Fatal("publication pass never reached the transport boundary")
	}

	unrelated := p.submitUnrelatedRun(t, "run-unrelated-production")
	result, err := p.workflow.Reconcile(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("reconcile pass beside a parked publication = %#v", result)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmissionRecord(p.ctx, unrelated)
		return err
	}); err != nil {
		t.Fatalf("unrelated invocation %q did not advance: %v", unrelated, err)
	}

	release()
	if err := <-publication; err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

// TestShutdownEndsAParkedProductionPublicationWithoutLosingItsTask proves the
// publication loop shuts down like the reconcile loop: cancellation ends the
// parked worker, and the durable task survives for the next process to finish.
func TestShutdownEndsAParkedProductionPublicationWithoutLosingItsTask(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	entered, release := p.transport.blockNextFetch()
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(p.ctx)
	loop := make(chan error, 1)
	go func() {
		loop <- p.workflow.RunProductionPublications(ctx, time.Millisecond)
	}()
	select {
	case <-entered:
	case err := <-loop:
		t.Fatalf("publication loop returned without reaching the transport: %v", err)
	case <-time.After(publicationBoundaryWait):
		t.Fatal("publication loop never reached the transport boundary")
	}

	cancel()
	select {
	case err := <-loop:
		if err != nil {
			t.Fatalf("canceled publication loop = %v", err)
		}
	case <-time.After(publicationBoundaryWait):
		t.Fatal("canceled publication loop leaked its worker")
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("interrupted publication caused effects: %d refs, %d PRs", refs, prs)
	}
	var task store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		return err
	}); err != nil {
		t.Fatalf("publication task did not survive shutdown: %v", err)
	}
	if task.Dispatched() {
		t.Fatal("interrupted publication dispatched its durable task")
	}

	// Restart: a fresh engine over the same store finishes the surviving task.
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

// TestProductionPublicationRecordsWorkUnitPRBinding (§5.18, issue #443):
// once a declared run's publication reaches its ready state, the daemon
// records the unit's exact PR binding from first-party facts — the
// admitted base revision, the published PR number, and the publication
// head — and a converged re-pass restates the same record instead of
// duplicating or conflicting.
func TestProductionPublicationRecordsWorkUnitPRBinding(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
	declaration := *p.declaration

	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
	if result.LastPRNumber <= 0 {
		t.Fatalf("reconcile result carries no PR number: %+v", result)
	}
	message := p.transport.pushedCommitMessage()
	if subject, _, _ := strings.Cut(message, "\n"); subject != "Publish run-production-publication (#443)" {
		t.Fatalf("issue-bound fallback subject = %q", subject)
	}

	readBinding := func() domain.WorkUnitPRBinding {
		t.Helper()
		var binding domain.WorkUnitPRBinding
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			binding, err = tx.GetWorkUnitPRBinding(p.ctx, declaration.ID)
			return err
		}); err != nil {
			t.Fatalf("read binding: %v", err)
		}
		return binding
	}
	binding := readBinding()
	if binding.PRNumber != result.LastPRNumber || binding.Repo != fakePublicationRepo ||
		binding.RepositoryID != p.profile.RepositoryID || binding.HeadSHA != p.replay.HeadSHA ||
		binding.BaseRef == "" {
		t.Fatalf("binding = %+v, want the published PR's first-party coordinates", binding)
	}

	// A converged replay of the published state restates the same record.
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if again := readBinding(); again != binding {
		t.Fatalf("replay churned the binding: %+v vs %+v", again, binding)
	}
}

// recordSupersedingReviewProfile records (and thereby activates as latest) a
// profile revision identical to the harness profile except its review
// configuration digest; widen additionally flips a non-review trust field to
// exercise the supersession gate's refusal.
func recordSupersedingReviewProfile(
	t *testing.T, p *productionPublicationHarness, configDigest domain.Digest, widen bool,
) domain.AutomationTrustProfile {
	t.Helper()
	in := domain.AutomationTrustProfileInput{
		Repo:                       p.profile.Repo,
		RepositoryID:               p.profile.RepositoryID,
		PRExecution:                p.profile.PRExecution,
		CandidateAutomationChanges: p.profile.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   p.profile.PRGitHubTokenPermissions,
		AllowOIDC:                  p.profile.AllowOIDC,
		AllowEnvironmentSecrets:    p.profile.AllowEnvironmentSecrets,
		AllowSecretBearingPRJobs:   p.profile.AllowSecretBearingPRJobs,
		AllowSelfHostedCI:          p.profile.AllowSelfHostedCI,
		AllowPullRequestTarget:     p.profile.AllowPullRequestTarget,
		AllowReusableWorkflows:     p.profile.AllowReusableWorkflows,
		AllowPackagePublishing:     p.profile.AllowPackagePublishing,
		AllowArtifactConsumers:     p.profile.AllowArtifactConsumers,
		CommitPlan:                 p.profile.CommitPlan,
		MessageRuleset:             p.profile.MessageRuleset,
		WorkflowAuditDigest:        p.profile.WorkflowAuditDigest,
		Review: domain.ReviewSettings{
			Mode: p.profile.Review.Mode, ConfigDigest: configDigest,
		},
		ProtectedPaths: p.profile.ProtectedPaths,
	}
	if widen {
		in.AllowSelfHostedCI = !in.AllowSelfHostedCI
	}
	profile, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(p.ctx, profile, p.now)
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

// parkOnSupersededReviewConfiguration drives the harness run into the parked
// configuration state under a new effective digest and returns the raised
// recovery item's snapshot.
func parkOnSupersededReviewConfiguration(
	t *testing.T, p *productionPublicationHarness, effective domain.Digest,
) signet.AttentionItemSnapshot {
	t.Helper()
	p.reviewConfigurationDigest = effective
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("park on superseded configuration = %#v, %v", result, err)
	}
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("raise configuration recovery item = %#v, %v", result, err)
	}
	snapshot, err := p.attention.GetAttentionItem(
		p.ctx, productionReviewItemIDForTest(p.runID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Item.Type != domain.AttentionReviewConfiguration ||
		snapshot.Item.Status != domain.StatusOpen ||
		snapshot.Item.ReviewConfigurationRecovery == nil {
		t.Fatalf("parked configuration item = %#v", snapshot.Item)
	}
	return snapshot
}

func submitOnParkedConfigurationItem(
	t *testing.T, p *productionPublicationHarness,
	snapshot signet.AttentionItemSnapshot, commandID string, action domain.Action,
) error {
	t.Helper()
	deviceID := domain.DeviceID("device-config-recovery")
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Configuration recovery device",
			Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceID,
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: snapshot.Item.ID, ItemVersion: snapshot.Item.ItemVersion,
			PRHeadSHA:       snapshot.Item.PRHeadSHA,
			ArtifactDigests: snapshot.Item.ArtifactDigests,
			Action:          action,
		},
	})
	return err
}

// TestProductionReviewConfigurationAdoptionResumesRun is issue #611's core
// acceptance: a run parked on a superseded reviewer configuration resumes
// after one operator-authorized adoption of a review-configuration-only
// profile supersession, with the parked failure row and the superseded
// profile revision byte-identical afterward and no terminal record written
// while parked.
func TestProductionReviewConfigurationAdoptionResumesRun(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	firstID := engine.ProductionReviewInvocationID(p.runID, 1)
	var original domain.ReviewFailure
	var originalDigest domain.Digest
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		original, err = tx.GetReviewFailure(p.ctx, firstID)
		if err != nil {
			return err
		}
		originalDigest, err = tx.ReviewFailureBodyDigest(p.ctx, firstID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	superseding := recordSupersedingReviewProfile(t, p, effective, false)
	if err := submitOnParkedConfigurationItem(
		t, p, snapshot, "adopt-config-round-1", domain.ActionAdoptReviewConfiguration,
	); err != nil {
		t.Fatalf("submit adoption: %v", err)
	}

	p.now = p.now.Add(time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			ConfigurationDigest: effective,
			CompletedAt:         p.now, CompletionEvidence: productionDigest([]byte("adopted clean review")),
		},
	})
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("adopted review = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.GetReviewFailure(p.ctx, firstID)
		if err != nil {
			return err
		}
		digest, err := tx.ReviewFailureBodyDigest(p.ctx, firstID)
		if err != nil {
			return err
		}
		if failure != original || digest != originalDigest {
			t.Fatalf("original failure changed: %#v/%s, want %#v/%s",
				failure, digest, original, originalDigest)
		}
		superseded, err := tx.GetTrustProfile(p.ctx, p.profile.ProfileDigest)
		if err != nil {
			return err
		}
		if superseded.ProfileDigest != p.profile.ProfileDigest {
			t.Fatalf("superseded profile changed: %#v", superseded)
		}
		latest, err := tx.LatestTrustProfile(p.ctx, p.profile.Repo)
		if err != nil {
			return err
		}
		if latest.ProfileDigest != superseding.ProfileDigest {
			t.Fatalf("latest profile = %s, want adopted %s",
				latest.ProfileDigest, superseding.ProfileDigest)
		}
		record, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if record.Round != 2 || record.InvocationID != secondID {
			t.Fatalf("adopted review record = %#v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionAdoptedReviewReplayPublishesWithoutReparking pins durable
// crash recovery across an adoption (issue #611, Codex round 1): the adopted
// round's clean record persists, the publishing push then fails transiently,
// and a restarted daemon replays the record and publishes. Before the fix,
// the configuration gate consulted the adoption only while the parked failure
// outranked the latest review row, so this replay re-recorded a configuration
// failure and re-parked an already recovered run.
func TestProductionAdoptedReviewReplayPublishesWithoutReparking(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	fault := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = fault
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	recordSupersedingReviewProfile(t, p, effective, false)
	if err := submitOnParkedConfigurationItem(
		t, p, snapshot, "adopt-config-replay", domain.ActionAdoptReviewConfiguration,
	); err != nil {
		t.Fatalf("submit adoption: %v", err)
	}

	p.now = p.now.Add(time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			ConfigurationDigest: effective,
			CompletedAt:         p.now, CompletionEvidence: productionDigest([]byte("adopted clean review")),
		},
	})
	p.transport.failNextPush()
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("transient publishing push after adoption = %#v, %v", result, err)
	}
	if fault.requestCalls != 1 {
		t.Fatalf("adopted round review requests = %d, want 1", fault.requestCalls)
	}
	// The adopted round's clean record persisted before the failed push.
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Outcome != domain.ReviewClean || review.Round != 2 {
			t.Fatalf("persisted adopted review before push = %#v", review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The daemon restarts, so the later pass replays from durable state alone.
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.now = p.now.Add(time.Minute)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 ||
		result.LastPRNumber == 0 {
		t.Fatalf("adopted record-replay publish = %#v, %v", result, err)
	}
	if fault.requestCalls != 1 {
		t.Fatalf("record replay invoked a new review round: requests = %d", fault.requestCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.Round != 1 || failure.Class != domain.ReviewFailureConfiguration {
			t.Fatalf("replay recorded a new failure = %#v", failure)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

// TestProductionReviewConfigurationStopConcludesParkedRun pins the operator's
// other exit: concluding the parked item without an effective adoption ends
// the run the way #527 decision 3 always did for this class.
func TestProductionReviewConfigurationStopConcludesParkedRun(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	if err := submitOnParkedConfigurationItem(
		t, p, snapshot, "stop-config-round-1", domain.ActionStop,
	); err != nil {
		t.Fatalf("submit stop: %v", err)
	}
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.BlockedItemsCreated != 1 {
		t.Fatalf("stopped parked run = %#v, %v", result, err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 0 {
		t.Fatalf("stopped run replay = %#v, %v", result, err)
	}
}

// TestProductionReviewConfigurationAdoptionRejectsTrustWidening pins the
// fail-closed arm at the decision boundary: an adoption whose only available
// superseding revision changes trust beyond the review configuration digest
// rolls the whole decision back, leaving the item open and the run parked.
func TestProductionReviewConfigurationAdoptionRejectsTrustWidening(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	recordSupersedingReviewProfile(t, p, effective, true)
	err := submitOnParkedConfigurationItem(
		t, p, snapshot, "adopt-config-widened", domain.ActionAdoptReviewConfiguration,
	)
	if !errors.Is(err, domain.ErrReviewConfigSupersessionInvalid) {
		t.Fatalf("widened adoption = %v, want %v", err, domain.ErrReviewConfigSupersessionInvalid)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, snapshot.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen {
		t.Fatalf("rejected adoption concluded the item: %#v", item.Item)
	}
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("run left the park after a rejected adoption = %#v, %v", result, err)
	}
}

// TestProductionReviewConfigurationAdoptionOutlivedByNewerProfileStaysParked
// pins the presence-parks contract: once the operator has adopted, a later
// profile activation makes the recorded adoption ineffective, and the run
// must stay parked (no terminal record, no resume) rather than treating the
// concluded item as a decline.
func TestProductionReviewConfigurationAdoptionOutlivedByNewerProfileStaysParked(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	recordSupersedingReviewProfile(t, p, effective, false)
	if err := submitOnParkedConfigurationItem(
		t, p, snapshot, "adopt-config-outlived", domain.ActionAdoptReviewConfiguration,
	); err != nil {
		t.Fatalf("submit adoption: %v", err)
	}
	// The operator activates yet another revision before the run resumes; the
	// recorded adoption no longer names the latest profile.
	recordSupersedingReviewProfile(
		t, p, domain.Digest("sha256:"+strings.Repeat("f", 64)), false)
	for tick := 0; tick < 3; tick++ {
		result, err := p.reconcileLanes()
		if err != nil || result.PublicationTasksCompleted != 0 ||
			result.BlockedItemsCreated != 0 || result.ReadyItemsCreated != 0 {
			t.Fatalf("outlived adoption tick %d = %#v, %v", tick, result, err)
		}
	}
}

// TestProductionReviewConfigurationAdoptionRejectsIneffectiveTarget pins the
// decision-time effectiveness gate end to end (issue #611, Codex round 4):
// adopting while the latest activated revision does not approve the daemon's
// effective configuration is rejected with the item left open, so the
// operator activates the matching revision, retries, and the run resumes.
// Without the gate, the accepted-but-ineffective adoption would conclude the
// item while the one-adoption-per-failure binding blocks a corrected retry,
// parking the run permanently.
func TestProductionReviewConfigurationAdoptionRejectsIneffectiveTarget(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	effective := domain.Digest("sha256:" + strings.Repeat("d", 64))
	snapshot := parkOnSupersededReviewConfiguration(t, p, effective)
	// The activated revision approves a different configuration than the
	// daemon effectively runs; adopting it could never grant authority.
	recordSupersedingReviewProfile(
		t, p, domain.Digest("sha256:"+strings.Repeat("e", 64)), false)
	err := submitOnParkedConfigurationItem(
		t, p, snapshot, "adopt-config-ineffective", domain.ActionAdoptReviewConfiguration,
	)
	if !errors.Is(err, domain.ErrReviewConfigAdoptionIneffective) {
		t.Fatalf("ineffective adoption = %v, want %v", err, domain.ErrReviewConfigAdoptionIneffective)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, snapshot.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen {
		t.Fatalf("rejected adoption concluded the item: %#v", item.Item)
	}
	if result, err := p.reconcileLanes(); err != nil ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("run left the park after a rejected adoption = %#v, %v", result, err)
	}

	// The operator activates the matching revision and retries on the still
	// open item; the adopted round then reviews clean and publishes.
	recordSupersedingReviewProfile(t, p, effective, false)
	if err := submitOnParkedConfigurationItem(
		t, p, item, "adopt-config-effective", domain.ActionAdoptReviewConfiguration,
	); err != nil {
		t.Fatalf("submit retry adoption: %v", err)
	}
	p.now = p.now.Add(time.Second)
	secondID := engine.ProductionReviewInvocationID(p.runID, 2)
	p.reviewer.Script(secondID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			ConfigurationDigest: effective,
			CompletedAt:         p.now, CompletionEvidence: productionDigest([]byte("retry adopted clean review")),
		},
	})
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 1 {
		t.Fatalf("retried adoption resume = %#v, %v", result, err)
	}
}

// TestProductionReviewLegacyDisputeItemStillTerminalizes covers the upgrade
// seam: a pre-#611 daemon that raised the terminalizing review_dispute for a
// configuration failure and crashed before completing the task left that item
// at the round's deterministic identity. The parked path honors the contract
// that item presented instead of failing closed against its shape.
func TestProductionReviewLegacyDisputeItemStillTerminalizes(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.reviewConfigurationDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("record configuration failure: %v", err)
	}
	runID := p.runID
	legacy, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReviewItemIDForTest(p.runID, 1), ProjectID: p.projectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(p.runID), RunID: &runID,
		},
		Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal,
		Reason:            "Codex review stopped because of a configuration failure.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss, domain.ActionStop},
		PRHeadSHA:         p.replay.HeadSHA, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(p.ctx, legacy)
	}); err != nil {
		t.Fatal(err)
	}
	result, err := p.reconcileLanes()
	if err != nil || result.PublicationTasksCompleted != 1 || result.BlockedItemsCreated != 1 {
		t.Fatalf("legacy dispute conclusion = %#v, %v", result, err)
	}
}
